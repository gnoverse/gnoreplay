package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	sm "github.com/gnolang/gno/tm2/pkg/bft/state"
	"github.com/gnolang/gno/tm2/pkg/bft/store"
	bft "github.com/gnolang/gno/tm2/pkg/bft/types"
	dbm "github.com/gnolang/gno/tm2/pkg/db"
	"github.com/gnolang/gno/tm2/pkg/sdk"
)

// Source is the recorded chain being replayed: its blocks, and the state DB
// holding the ABCI responses the original binary produced for each height.
type Source struct {
	Blocks  *store.BlockStore
	StateDB dbm.DB
}

type replayOptions struct {
	Genesis     *bft.GenesisDoc
	ToHeight    int64 // last height to replay; 0 means the source tip
	StopOnFirst bool  // stop after the first block with a result or app hash divergence
	MaxDiffs    int   // diffs kept in the report; counts stay exact
	Logger      *slog.Logger
}

// replay re-executes every recorded block of src on app and compares each
// response against the recorded one. It keeps going after a divergence, so a
// single run lists every affected tx; diffs found once the app hash has
// diverged are flagged, as they may be a consequence of an earlier one.
func replay(src Source, app abci.Application, opts replayOptions) (*Report, error) {
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

	// Genesis.
	validators := make([]*bft.Validator, len(genDoc.Validators))
	for i, val := range genDoc.Validators {
		validators[i] = bft.NewValidator(val.PubKey, val.Power)
	}
	csParams := genDoc.ConsensusParams
	initRes := app.InitChain(abci.RequestInitChain{
		Time:            genDoc.GenesisTime,
		ChainID:         genDoc.ChainID,
		ConsensusParams: &csParams,
		Validators:      bft.NewValidatorSet(validators).ABCIValidatorUpdates(),
		AppState:        genDoc.AppState,
		InitialHeight:   genDoc.InitialHeight,
	})
	if initRes.Error != nil {
		return nil, fmt.Errorf("InitChain: %v", initRes.Error)
	}
	// The handshaker stores genesis tx results at height 0.
	if want, err := loadResponses(src.StateDB, 0); err != nil {
		return nil, err
	} else if want != nil {
		r.compareTxs(0, nil, want.DeliverTxs, initRes.TxResponses)
	}
	r.Timing.GenesisSeconds = time.Since(start).Seconds()
	logger.Info("genesis replayed", "txs", len(initRes.TxResponses), "elapsed", time.Since(start))

	baseApp, _ := app.(*sdk.BaseApp)
	blocksStart := time.Now()
	lastLog := time.Now()

	for height := firstHeight; height <= tip; height++ {
		block := src.Blocks.LoadBlock(height)
		if block == nil {
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

		commitInfo := lastCommitInfo(src.StateDB, block, firstHeight)
		app.BeginBlock(abci.RequestBeginBlock{
			Hash:           block.Hash(),
			Header:         block.Header.Copy(),
			LastCommitInfo: &commitInfo,
		})
		got := make([]abci.ResponseDeliverTx, len(block.Txs))
		for i, tx := range block.Txs {
			got[i] = app.DeliverTx(abci.RequestDeliverTx{Tx: tx})
		}
		endRes := app.EndBlock(abci.RequestEndBlock{Height: height})
		appHash := app.Commit().Data

		diverged := false
		if want != nil {
			diverged = r.compareTxs(height, block.Txs, want.DeliverTxs, got)
			if !sameValidatorUpdates(want.EndBlock.ValidatorUpdates, endRes.ValidatorUpdates) {
				r.addBlockDiff(height, "validator updates differ")
				diverged = true
			}
		} else {
			r.MissingResponses++
		}
		if wantHash := recordedAppHash(src, height); wantHash != nil && !bytes.Equal(wantHash, appHash) {
			if r.FirstAppHashMismatch == 0 {
				r.FirstAppHashMismatch = height
				r.addBlockDiff(height, fmt.Sprintf("app hash %X, recorded %X", appHash, wantHash))
			}
			diverged = true
		}
		r.Blocks++
		r.Txs += int64(len(block.Txs))

		if diverged && opts.StopOnFirst {
			r.ToHeight = height
			break
		}
		if time.Since(lastLog) > 30*time.Second {
			lastLog = time.Now()
			logger.Info("replaying", "height", height, "tip", tip,
				"blocks/s", float64(r.Blocks)/time.Since(blocksStart).Seconds(),
				"result_diffs", r.Counts.Result, "gas_diffs", r.Counts.Gas)
		}
	}

	r.Timing.BlocksSeconds = time.Since(blocksStart).Seconds()
	r.Timing.TotalSeconds = time.Since(start).Seconds()
	return r, nil
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

// lastCommitInfo mirrors getBeginBlockLastCommitInfo in tm2/pkg/bft/state,
// without its panics: a source missing validator history still replays.
func lastCommitInfo(stateDB dbm.DB, block *bft.Block, initialHeight int64) abci.LastCommitInfo {
	info := abci.LastCommitInfo{Round: int32(block.LastCommit.Round())}
	if block.Height <= initialHeight {
		return info
	}
	vals, err := sm.LoadValidators(stateDB, block.Height-1)
	if err != nil {
		return info
	}
	info.Votes = make([]abci.VoteInfo, len(vals.Validators))
	for i, val := range vals.Validators {
		var vote *bft.CommitSig
		if i < len(block.LastCommit.Precommits) {
			vote = block.LastCommit.Precommits[i]
		}
		info.Votes[i] = abci.VoteInfo{
			Address:         val.Address,
			Power:           val.VotingPower,
			SignedLastBlock: vote != nil,
		}
	}
	return info
}

func sameValidatorUpdates(a, b []abci.ValidatorUpdate) bool {
	return bytes.Equal(
		amino.MustMarshal(abci.ResponseEndBlock{ValidatorUpdates: a}),
		amino.MustMarshal(abci.ResponseEndBlock{ValidatorUpdates: b}),
	)
}
