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

func TestReplay(t *testing.T) {
	if testing.Short() {
		// Boots a node and replays its chain three times.
		t.Skip("skipping in -short mode")
	}
	src, genDoc := recordChain(t)
	tip := src.Blocks.Height()

	t.Run("same binary", func(t *testing.T) {
		r, err := replay(src, newTestApp(t), replayOptions{Genesis: genDoc})
		require.NoError(t, err)
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
		r, err := replay(src, app, replayOptions{Genesis: genDoc})
		require.NoError(t, err)
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

		r, err := replay(src, newTestApp(t), replayOptions{Genesis: &forked})
		require.NoError(t, err)
		assert.EqualValues(t, 1, r.FirstAppHashMismatch)
		assert.Equal(t, 1, r.Counts.Block, "only the first app hash mismatch is listed")

		stopped, err := replay(src, newTestApp(t), replayOptions{Genesis: &forked, StopOnFirst: true})
		require.NoError(t, err)
		assert.Equal(t, r.FirstAppHashMismatch, stopped.ToHeight)
		assert.EqualValues(t, 1, stopped.Blocks)
	})
}

func TestLoadResponsesMissing(t *testing.T) {
	res, err := loadResponses(memdb.NewMemDB(), 5)
	require.NoError(t, err)
	assert.Nil(t, res)
}
