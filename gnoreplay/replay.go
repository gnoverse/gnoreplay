package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	"github.com/gnolang/gno/tm2/pkg/bft/appconn"
	cs "github.com/gnolang/gno/tm2/pkg/bft/consensus"
	"github.com/gnolang/gno/tm2/pkg/bft/mempool/mock"
	"github.com/gnolang/gno/tm2/pkg/bft/proxy"
	sm "github.com/gnolang/gno/tm2/pkg/bft/state"
	"github.com/gnolang/gno/tm2/pkg/bft/store"
	bft "github.com/gnolang/gno/tm2/pkg/bft/types"
	dbm "github.com/gnolang/gno/tm2/pkg/db"
	"github.com/gnolang/gno/tm2/pkg/db/memdb"
	"github.com/gnolang/gno/tm2/pkg/log"
	"github.com/gnolang/gno/tm2/pkg/sdk"
)

// Source is the recorded chain being replayed: its blocks, and the state DB
// holding the ABCI responses the original binary produced for each height.
type Source struct {
	Blocks  *store.BlockStore
	StateDB dbm.DB
}

// Block sync's flush points (tm2/pkg/bft/blockchain): at least every second,
// and when the app holds more than 64 MiB or asks for one.
const (
	flushPointInterval = time.Second
	flushPointBytes    = 64 << 20
)

// deferredCommitter is tm2's abci.DeferredCommitter (gnolang/gno#6334),
// declared here so that the tool still builds on trees without it.
type deferredCommitter interface {
	DeferCommits(on bool) bool
	WriteCommits() error
	HeldBytes() int64
	NeedsFlushPoint() bool
}

type replayOptions struct {
	Genesis     *bft.GenesisDoc
	ToHeight    int64 // last height to replay; 0 means the source tip
	StopOnFirst bool  // stop after the first block with a result or app hash divergence
	MaxDiffs    int   // diffs kept in the report; counts stay exact
	Logger      *slog.Logger
}

// replay re-executes every recorded block of src on app as a syncing node
// does: tm2's genesis handshake, then its ApplyBlock for each block, which
// keeps tm2's state in stateDB (empty at the start). It compares each
// response against the recorded one, and keeps going after a divergence, so
// a single run lists every affected tx; diffs found once the app hash has
// diverged are flagged, as they may be a consequence of an earlier one.
func replay(src Source, app abci.Application, stateDB dbm.DB, opts replayOptions) (*Report, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	genDoc := opts.Genesis
	start := time.Now()

	firstHeight := genDoc.InitialHeight
	if firstHeight == 0 {
		firstHeight = 1
	}
	tip := src.Blocks.Height()
	if opts.ToHeight > 0 && opts.ToHeight < tip {
		tip = opts.ToHeight
	}

	r := newReport(genDoc.ChainID, firstHeight, tip, opts.MaxDiffs)

	baseApp, _ := app.(*sdk.BaseApp)
	genesis := &genesisCheck{Application: app}
	// tm2 logs every block, and returns its errors: those are what we report.
	tmLogger := log.NewNoopLogger()
	proxyApp := appconn.NewAppConns(proxy.NewLocalClientCreator(genesis))
	proxyApp.SetLogger(tmLogger)
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("start app connections: %w", err)
	}
	defer proxyApp.Stop()

	// Genesis, as a node's first start does it: the handshake, on an empty
	// block store, sends InitChain and stores the genesis tx results at
	// height 0.
	state, err := sm.LoadStateFromDBOrGenesisDoc(stateDB, genDoc)
	if err != nil {
		return nil, err
	}
	handshaker := cs.NewHandshaker(stateDB, state, store.NewBlockStore(memdb.NewMemDB()), genDoc)
	handshaker.SetLogger(tmLogger)
	if err := handshaker.Handshake(proxyApp); err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if genesis.err != nil {
		return nil, fmt.Errorf("InitChain: %v", genesis.err)
	}
	state = sm.LoadState(stateDB)
	gotGenesis, err := sm.LoadABCIResponses(stateDB, 0)
	if err != nil {
		return nil, err
	}
	if want, err := loadResponses(src.StateDB, 0); err != nil {
		return nil, err
	} else if want != nil {
		r.compareTxs(0, nil, want.DeliverTxs, gotGenesis.DeliverTxs)
	}
	r.Timing.GenesisSeconds = time.Since(start).Seconds()
	logger.Info("genesis replayed", "txs", len(gotGenesis.DeliverTxs), "elapsed", time.Since(start))

	blockExec := sm.NewBlockExecutor(stateDB, tmLogger, proxyApp.Consensus(), mock.Mempool{})
	blocksStart := time.Now()
	lastLog := time.Now()

	// Where the app can, block sync holds its commits in memory and writes
	// them at flush points: so does the replay, as how the app holds them
	// changes what its blocks read. tm2's state is written as before: holding
	// it only matters to a node's crash recovery.
	deferred, _ := app.(deferredCommitter)
	if deferred != nil && !deferred.DeferCommits(true) {
		deferred = nil
	}
	lastFlush := time.Now()
	flushPoint := func(height int64) error {
		if err := deferred.WriteCommits(); err != nil {
			return fmt.Errorf("write the app's commits up to height %d: %w", height, err)
		}
		r.FlushPoints++
		lastFlush = time.Now()
		return nil
	}

	for height := firstHeight; height <= tip; height++ {
		block := src.Blocks.LoadBlock(height)
		meta := src.Blocks.LoadBlockMeta(height)
		if block == nil || meta == nil {
			return nil, fmt.Errorf("block %d missing from blockstore", height)
		}
		want, err := loadResponses(src.StateDB, height)
		if err != nil {
			return nil, err
		}

		// Governance halts recorded in history must not stop the replay.
		if baseApp != nil {
			baseApp.SetHaltHeight(0)
		}

		next, err := applyBlock(blockExec, state, meta.BlockID, block)
		if err != nil {
			// A node built from this code would stop here too, and lose
			// what the app holds.
			r.addBlockDiff(height, fmt.Sprintf("block not applied: %v", err))
			r.ToHeight = height
			deferred = nil
			break
		}
		got, err := sm.LoadABCIResponses(stateDB, height)
		if err != nil {
			return nil, err
		}

		var resultsDiverged, valsDiverged, paramsDiverged bool
		if want != nil {
			resultsDiverged = r.compareTxs(height, block.Txs, want.DeliverTxs, got.DeliverTxs)
			if !sameValidatorUpdates(want.EndBlock.ValidatorUpdates, got.EndBlock.ValidatorUpdates) {
				r.addBlockDiff(height, "validator updates differ")
				valsDiverged = true
			}
			if !sameConsensusParams(want.EndBlock.ConsensusParams, got.EndBlock.ConsensusParams) {
				r.addBlockDiff(height, "consensus params updates differ")
				paramsDiverged = true
			}
		} else {
			r.MissingResponses++
		}
		wantHash := recordedAppHash(src, height)
		hashDiverged := wantHash != nil && !bytes.Equal(wantHash, next.AppHash)
		if hashDiverged && r.FirstAppHashMismatch == 0 {
			r.FirstAppHashMismatch = height
			r.addBlockDiff(height, fmt.Sprintf("app hash %X, recorded %X", next.AppHash, wantHash))
		}
		r.Blocks++
		r.Txs += int64(len(block.Txs))

		if opts.StopOnFirst && (resultsDiverged || valsDiverged || paramsDiverged || hashDiverged) {
			r.ToHeight = height
			break
		}

		// ApplyBlock checks the next block's header against tm2's state.
		// Where this block diverged, the state takes what the chain recorded
		// (the app keeps its own), so that the replay goes on; any other
		// disagreement is a change in tm2 itself, and the next block is not
		// applied.
		if hashDiverged {
			next.AppHash = wantHash
		}
		if resultsDiverged {
			next.LastResultsHash = want.ResultsHash()
		}
		if valsDiverged || paramsDiverged {
			if valsDiverged {
				if next.NextValidators, err = sm.LoadValidators(src.StateDB, height+2); err != nil {
					return nil, err
				}
				next.LastHeightValidatorsChanged = height + 2
			}
			if paramsDiverged {
				if next.ConsensusParams, err = sm.LoadConsensusParams(src.StateDB, height+1); err != nil {
					return nil, err
				}
				next.LastHeightConsensusParamsChanged = height + 1
			}
			// Saved whole at their change heights, where tm2 loads them from.
			sm.SaveState(stateDB, next)
		}
		state = next

		if deferred != nil && (time.Since(lastFlush) >= flushPointInterval ||
			deferred.HeldBytes() > flushPointBytes || deferred.NeedsFlushPoint()) {
			if err := flushPoint(height); err != nil {
				return nil, err
			}
		}

		if time.Since(lastLog) > 30*time.Second {
			lastLog = time.Now()
			logger.Info("replaying", "height", height, "tip", tip,
				"blocks/s", float64(r.Blocks)/time.Since(blocksStart).Seconds(),
				"result_diffs", r.Counts.Result, "gas_diffs", r.Counts.Gas)
		}
	}

	// As block sync does before it switches to consensus.
	if deferred != nil {
		if err := flushPoint(state.LastBlockHeight); err != nil {
			return nil, err
		}
	}
	r.Timing.BlocksSeconds = time.Since(blocksStart).Seconds()
	r.Timing.TotalSeconds = time.Since(start).Seconds()
	return r, nil
}

// applyBlock is blockExec.ApplyBlock, with a panic returned as an error: the
// node would stop at the block too. One is an iterator over the app's
// database inside a block while its commits are held.
func applyBlock(blockExec *sm.BlockExecutor, state sm.State, id bft.BlockID, block *bft.Block) (next sm.State, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return blockExec.ApplyBlock(state, id, block)
}

// loadResponses returns the recorded responses for height, or nil if none
// were stored (e.g. a node that was state-synced past that height).
func loadResponses(db dbm.DB, height int64) (*sm.ABCIResponses, error) {
	res, err := sm.LoadABCIResponses(db, height)
	if errors.As(err, &sm.NoABCIResponsesForHeightError{}) {
		return nil, nil
	}
	return res, err
}

// recordedAppHash is the app hash the chain committed after height: the next
// block's header carries it. At the source tip it comes from the saved state.
func recordedAppHash(src Source, height int64) []byte {
	if meta := src.Blocks.LoadBlockMeta(height + 1); meta != nil {
		return meta.Header.AppHash
	}
	if state := sm.LoadState(src.StateDB); state.LastBlockHeight == height {
		return state.AppHash
	}
	return nil
}

// genesisCheck keeps InitChain's error, which the handshake ignores: a
// genesis that fails to load stops the replay rather than diverging at every
// block.
type genesisCheck struct {
	abci.Application
	err error
}

func (a *genesisCheck) InitChain(req abci.RequestInitChain) abci.ResponseInitChain {
	res := a.Application.InitChain(req)
	a.err = res.Error
	return res
}

func sameValidatorUpdates(a, b []abci.ValidatorUpdate) bool {
	return bytes.Equal(
		amino.MustMarshal(abci.ResponseEndBlock{ValidatorUpdates: a}),
		amino.MustMarshal(abci.ResponseEndBlock{ValidatorUpdates: b}),
	)
}

func sameConsensusParams(a, b *abci.ConsensusParams) bool {
	return bytes.Equal(
		amino.MustMarshal(abci.ResponseEndBlock{ConsensusParams: a}),
		amino.MustMarshal(abci.ResponseEndBlock{ConsensusParams: b}),
	)
}
