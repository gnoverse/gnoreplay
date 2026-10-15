package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gnolang/gno/gno.land/pkg/gnoclient"
	"github.com/gnolang/gno/gno.land/pkg/gnoland"
	"github.com/gnolang/gno/gno.land/pkg/gnoland/ugnot"
	"github.com/gnolang/gno/gno.land/pkg/integration"
	"github.com/gnolang/gno/gno.land/pkg/sdk/vm"
	"github.com/gnolang/gno/gnovm/pkg/gnoenv"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	rpcclient "github.com/gnolang/gno/tm2/pkg/bft/rpc/client"
	"github.com/gnolang/gno/tm2/pkg/bft/store"
	bft "github.com/gnolang/gno/tm2/pkg/bft/types"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/ed25519"
	"github.com/gnolang/gno/tm2/pkg/crypto/keys"
	"github.com/gnolang/gno/tm2/pkg/db/memdb"
	"github.com/gnolang/gno/tm2/pkg/log"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// recordChain runs an in-memory node, sends a successful MsgRun, a panicking
// MsgRun and a bank send, and returns the recorded chain.
func recordChain(t *testing.T) (Source, *bft.GenesisDoc) {
	t.Helper()

	cfg := integration.TestingMinimalNodeConfig(gnoenv.RootDir())
	node, remote := integration.TestingInMemoryNode(t, log.NewNoopLogger(), cfg)

	kb := keys.NewInMemory()
	_, err := kb.CreateAccount(integration.DefaultAccount_Name, integration.DefaultAccount_Seed, "", "", 0, 0)
	require.NoError(t, err)
	rpc, err := rpcclient.NewHTTPClient(remote)
	require.NoError(t, err)
	client := gnoclient.Client{
		Signer: &gnoclient.SignerFromKeybase{
			Keybase: kb,
			Account: integration.DefaultAccount_Name,
			ChainID: cfg.Genesis.ChainID,
		},
		RPCClient: rpc,
	}
	caller := crypto.MustAddressFromString(integration.DefaultAccount_Address)
	txCfg := func(seq uint64) gnoclient.BaseTxCfg {
		return gnoclient.BaseTxCfg{
			GasFee:         ugnot.ValueString(10_000_000),
			GasWanted:      10_000_000,
			SequenceNumber: seq,
		}
	}
	run := func(body string) vm.MsgRun {
		return vm.MsgRun{
			Caller: caller,
			Package: &std.MemPackage{
				Name:  "main",
				Files: []*std.MemFile{{Name: "main.gno", Body: body}},
			},
		}
	}

	_, err = client.Run(txCfg(0), run(`package main
func main() { println("hello") }
`))
	require.NoError(t, err)
	_, err = client.Run(txCfg(1), run(`package main
func main() { panic("boom") }
`))
	require.Error(t, err, "the panicking tx must fail on the recorded chain")
	_, err = client.Send(txCfg(2), bank.MsgSend{
		FromAddress: caller,
		ToAddress:   crypto.AddressFromPreimage([]byte("recipient")),
		Amount:      std.MustParseCoins(ugnot.ValueString(1000)),
	})
	require.NoError(t, err)

	require.NoError(t, node.Stop())

	// The in-memory node keeps the blockstore, state and app in one DB.
	return Source{Blocks: store.NewBlockStore(cfg.DB), StateDB: cfg.DB}, cfg.Genesis
}

func newTestApp(t *testing.T) abci.Application {
	t.Helper()
	app, err := newApp(memdb.NewMemDB(), log.NewNoopLogger())
	require.NoError(t, err)
	return app
}

// mutatingApp simulates a binary whose tx handling changed.
type mutatingApp struct {
	abci.Application
	n      int
	mutate func(n int, res *abci.ResponseDeliverTx)
}

func (a *mutatingApp) DeliverTx(req abci.RequestDeliverTx) abci.ResponseDeliverTx {
	res := a.Application.DeliverTx(req)
	a.mutate(a.n, &res)
	a.n++
	return res
}

// validatorApp simulates a binary whose EndBlocker adds a validator at height.
type validatorApp struct {
	abci.Application
	height int64
}

func (a *validatorApp) EndBlock(req abci.RequestEndBlock) abci.ResponseEndBlock {
	res := a.Application.EndBlock(req)
	if req.Height == a.height {
		key := ed25519.GenPrivKey().PubKey()
		res.ValidatorUpdates = append(res.ValidatorUpdates, abci.ValidatorUpdate{Address: key.Address(), PubKey: key, Power: 1})
	}
	return res
}

// versionApp simulates a binary reporting another app version, which every
// recorded block header contradicts.
type versionApp struct{ abci.Application }

func (a versionApp) Info(req abci.RequestInfo) abci.ResponseInfo {
	res := a.Application.Info(req)
	res.AppVersion = "replay test"
	return res
}

// deferringApp simulates a binary that holds its commits in memory between
// flush points (gnolang/gno#6334): it asks for one after the block at
// flushAt, and its block at panicAt panics while it holds, as an iterator over
// the held writes does.
type deferringApp struct {
	abci.Application
	flushAt, panicAt int64
	holding          bool
	height           int64
	flushes          []int64 // the height of each flush point
}

func (a *deferringApp) DeferCommits(on bool) bool { a.holding = on; return true }
func (a *deferringApp) WriteCommits() error       { a.flushes = append(a.flushes, a.height); return nil }
func (a *deferringApp) HeldBytes() int64          { return 0 }
func (a *deferringApp) NeedsFlushPoint() bool     { return a.height == a.flushAt }

func (a *deferringApp) BeginBlock(req abci.RequestBeginBlock) abci.ResponseBeginBlock {
	a.height = req.Header.GetHeight()
	if a.holding && a.height == a.panicAt {
		panic("iterator over held writes")
	}
	return a.Application.BeginBlock(req)
}

func TestReplay(t *testing.T) {
	if testing.Short() {
		// Boots a node and replays its chain eight times.
		t.Skip("skipping in -short mode")
	}
	src, genDoc := recordChain(t)
	tip := src.Blocks.Height()

	t.Run("same binary", func(t *testing.T) {
		app := newTestApp(t)
		r, err := replay(src, app, memdb.NewMemDB(), replayOptions{Genesis: genDoc})
		require.NoError(t, err)
		if _, ok := app.(deferredCommitter); ok {
			assert.Positive(t, r.FlushPoints, "the app's commits are held, as in block sync")
		} else {
			assert.Zero(t, r.FlushPoints)
		}
		assert.Equal(t, tip, r.ToHeight)
		assert.EqualValues(t, tip, r.Blocks)
		assert.EqualValues(t, 3, r.Txs)
		assert.Zero(t, r.FirstAppHashMismatch)
		assert.Zero(t, r.Counts.Result)
		assert.Zero(t, r.Counts.Gas)
		assert.Zero(t, r.Counts.Block)
		assert.Empty(t, r.Diffs)
	})

	t.Run("changed results", func(t *testing.T) {
		app := &mutatingApp{Application: newTestApp(t), mutate: func(n int, res *abci.ResponseDeliverTx) {
			switch n {
			case 0: // the successful MsgRun now fails
				res.Error = abci.StringError("replay test")
			case 2: // the bank send uses more gas
				res.GasUsed += 7
			}
		}}
		r, err := replay(src, app, memdb.NewMemDB(), replayOptions{Genesis: genDoc})
		require.NoError(t, err)
		assert.EqualValues(t, tip, r.Blocks, "the next block validates against the recorded results hash")
		assert.Equal(t, 1, r.Counts.Result)
		assert.Equal(t, 1, r.Counts.Gas)
		// The panicking MsgRun fails the same way on replay: no diff for it.
		require.Len(t, r.Diffs, 2)

		res := r.Diffs[0]
		assert.Equal(t, kindResult, res.Kind)
		assert.Equal(t, []string{"vm/run " + integration.DefaultAccount_Address}, res.Msgs)
		assert.Empty(t, res.Recorded.Error)
		assert.Contains(t, res.Replayed.Error, "replay test")
		assert.NotEmpty(t, res.TxHash)

		gas := r.Diffs[1]
		assert.Equal(t, kindGas, gas.Kind)
		assert.Equal(t, []string{"bank/send"}, gas.Msgs)
		assert.Equal(t, gas.Recorded.GasUsed+7, gas.Replayed.GasUsed)
	})

	t.Run("diverged state", func(t *testing.T) {
		// Different genesis balances change the app hash from the first block.
		state := genDoc.AppState.(gnoland.GnoGenesisState)
		state.Balances = append(append([]gnoland.Balance{}, state.Balances...), gnoland.Balance{
			Address: crypto.AddressFromPreimage([]byte("extra")),
			Amount:  std.MustParseCoins(ugnot.ValueString(1)),
		})
		forked := *genDoc
		forked.AppState = state

		r, err := replay(src, newTestApp(t), memdb.NewMemDB(), replayOptions{Genesis: &forked})
		require.NoError(t, err)
		assert.EqualValues(t, tip, r.Blocks, "the next blocks validate against the recorded app hashes")
		assert.EqualValues(t, 1, r.FirstAppHashMismatch)
		assert.Equal(t, 1, r.Counts.Block, "only the first app hash mismatch is listed")

		stopped, err := replay(src, newTestApp(t), memdb.NewMemDB(), replayOptions{Genesis: &forked, StopOnFirst: true})
		require.NoError(t, err)
		assert.Equal(t, r.FirstAppHashMismatch, stopped.ToHeight)
		assert.EqualValues(t, 1, stopped.Blocks)
	})

	t.Run("changed validator updates", func(t *testing.T) {
		app := &validatorApp{Application: newTestApp(t), height: 1}
		r, err := replay(src, app, memdb.NewMemDB(), replayOptions{Genesis: genDoc})
		require.NoError(t, err)
		assert.EqualValues(t, tip, r.Blocks, "the next blocks validate against the recorded validator sets")
		require.Equal(t, 1, r.Counts.Block)
		assert.EqualValues(t, 1, r.Diffs[0].Height)
		assert.Equal(t, "validator updates differ", r.Diffs[0].Detail)
	})

	t.Run("held commits", func(t *testing.T) {
		require.GreaterOrEqual(t, tip, int64(2))
		app := &deferringApp{Application: newTestApp(t), flushAt: 1}
		r, err := replay(src, app, memdb.NewMemDB(), replayOptions{Genesis: genDoc})
		require.NoError(t, err)
		assert.EqualValues(t, tip, r.Blocks)
		assert.Empty(t, r.Diffs)
		// The one asked for, and the last; a slow block can add more.
		assert.Contains(t, app.flushes, int64(1))
		assert.Equal(t, tip, app.flushes[len(app.flushes)-1])
		assert.EqualValues(t, len(app.flushes), r.FlushPoints)

		app = &deferringApp{Application: newTestApp(t), panicAt: 2}
		r, err = replay(src, app, memdb.NewMemDB(), replayOptions{Genesis: genDoc})
		require.NoError(t, err)
		assert.EqualValues(t, 1, r.Blocks)
		assert.EqualValues(t, 2, r.ToHeight)
		require.Equal(t, 1, r.Counts.Block)
		assert.Equal(t, "block not applied: panic: iterator over held writes", r.Diffs[0].Detail)
		assert.NotContains(t, app.flushes, int64(2), "a node that stops at a block loses what the app holds")
	})

	t.Run("rejected block", func(t *testing.T) {
		r, err := replay(src, versionApp{newTestApp(t)}, memdb.NewMemDB(), replayOptions{Genesis: genDoc})
		require.NoError(t, err)
		assert.Zero(t, r.Blocks)
		assert.EqualValues(t, 1, r.ToHeight)
		require.Equal(t, 1, r.Counts.Block)
		assert.Contains(t, r.Diffs[0].Detail, "block not applied")
		assert.Contains(t, r.Diffs[0].Detail, "AppVersion")
	})
}

func TestLoadResponsesMissing(t *testing.T) {
	res, err := loadResponses(memdb.NewMemDB(), 5)
	require.NoError(t, err)
	assert.Nil(t, res)
}
