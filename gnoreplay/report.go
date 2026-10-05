package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gnolang/gno/gno.land/pkg/sdk/vm"
	"github.com/gnolang/gno/tm2/pkg/amino"
	abci "github.com/gnolang/gno/tm2/pkg/bft/abci/types"
	bft "github.com/gnolang/gno/tm2/pkg/bft/types"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// Diff kinds, from most to least severe.
const (
	// kindResult: Error, Data or Events differ. These feed LastResultsHash, so
	// the replaying binary would not reach consensus with the recorded chain.
	kindResult = "result"
	// kindGas: only GasUsed differs. Not consensus-relevant by itself, but a
	// tx that needs more gas than it asked for fails on a live chain.
	kindGas = "gas"
	// kindBlock: a block-level divergence (app hash, validator updates).
	kindBlock = "block"
)

type Report struct {
	ChainID    string `json:"chain_id"`
	FromHeight int64  `json:"from_height"`
	ToHeight   int64  `json:"to_height"`
	Blocks     int64  `json:"blocks"`
	Txs        int64  `json:"txs"`

	// FirstAppHashMismatch is the first height whose app hash differs from the
	// recorded one; 0 if none. Diffs at later heights have AfterDivergence set.
	FirstAppHashMismatch int64 `json:"first_app_hash_mismatch,omitempty"`
	// MissingResponses counts heights with no recorded responses to compare.
	MissingResponses int64 `json:"missing_responses,omitempty"`

	Counts struct {
		Result int `json:"result"`
		Gas    int `json:"gas"`
		Block  int `json:"block"`
		// Log counts txs whose only difference is the (nondeterministic) log.
		Log int `json:"log"`
	} `json:"counts"`

	Diffs []Diff `json:"diffs"`
	// Truncated is set when more diffs were found than MaxDiffs.
	Truncated bool `json:"truncated,omitempty"`

	Timing struct {
		GenesisSeconds float64 `json:"genesis_seconds"`
		BlocksSeconds  float64 `json:"blocks_seconds"`
		TotalSeconds   float64 `json:"total_seconds"`
	} `json:"timing"`

	maxDiffs int
}

type Diff struct {
	Kind   string `json:"kind"`
	Height int64  `json:"height"`
	// Index is the tx index in the block; -1 for block-level diffs.
	Index           int      `json:"index"`
	TxHash          string   `json:"tx_hash,omitempty"`
	Msgs            []string `json:"msgs,omitempty"`
	AfterDivergence bool     `json:"after_divergence,omitempty"`
	Detail          string   `json:"detail,omitempty"`
	Recorded        *Result  `json:"recorded,omitempty"`
	Replayed        *Result  `json:"replayed,omitempty"`
}

type Result struct {
	Error   string `json:"error,omitempty"`
	Log     string `json:"log,omitempty"`
	Data    string `json:"data,omitempty"`
	Events  int    `json:"events"`
	GasUsed int64  `json:"gas_used"`
}

func newReport(chainID string, from, to int64, maxDiffs int) *Report {
	return &Report{ChainID: chainID, FromHeight: from, ToHeight: to, Diffs: []Diff{}, maxDiffs: maxDiffs}
}

// compareTxs records a diff for each tx whose replayed response differs from
// the recorded one, and reports whether any consensus-relevant diff was found.
// txs may be nil (genesis txs are not in a block).
func (r *Report) compareTxs(height int64, txs bft.Txs, want, got []abci.ResponseDeliverTx) bool {
	if len(want) != len(got) {
		r.addBlockDiff(height, fmt.Sprintf("%d recorded tx responses, %d replayed", len(want), len(got)))
		return true
	}
	diverged := false
	for i := range want {
		w, g := want[i], got[i]
		kind := ""
		switch {
		case !bytes.Equal(bft.NewResultFromResponse(w).Bytes(), bft.NewResultFromResponse(g).Bytes()):
			kind = kindResult
			diverged = true
		case w.GasUsed != g.GasUsed:
			kind = kindGas
		case w.Log != g.Log:
			r.Counts.Log++
			continue
		default:
			continue
		}
		d := Diff{
			Kind:     kind,
			Height:   height,
			Index:    i,
			Recorded: toResult(w),
			Replayed: toResult(g),
		}
		if txs != nil {
			d.TxHash = fmt.Sprintf("%X", txs[i].Hash())
			d.Msgs = describeMsgs(txs[i])
		}
		if kind == kindGas {
			d.Detail = fmt.Sprintf("gas_used %d -> %d (%+d), gas_wanted %d", w.GasUsed, g.GasUsed, g.GasUsed-w.GasUsed, w.GasWanted)
		}
		r.add(d)
	}
	return diverged
}

func (r *Report) addBlockDiff(height int64, detail string) {
	r.add(Diff{Kind: kindBlock, Height: height, Index: -1, Detail: detail})
}

func (r *Report) add(d Diff) {
	switch d.Kind {
	case kindResult:
		r.Counts.Result++
	case kindGas:
		r.Counts.Gas++
	case kindBlock:
		r.Counts.Block++
	}
	// The app hash of height h is checked after its txs, so a mismatch
	// recorded at h does not make h's own diffs consequences of it.
	d.AfterDivergence = r.FirstAppHashMismatch > 0 && d.Height > r.FirstAppHashMismatch
	if r.maxDiffs > 0 && len(r.Diffs) >= r.maxDiffs {
		r.Truncated = true
		return
	}
	r.Diffs = append(r.Diffs, d)
}

const maxLogLen = 2000

func toResult(res abci.ResponseDeliverTx) *Result {
	out := &Result{
		Log:     truncate(res.Log, maxLogLen),
		Data:    truncate(string(res.Data), maxLogLen),
		Events:  len(res.Events),
		GasUsed: res.GasUsed,
	}
	if res.Error != nil {
		out.Error = fmt.Sprintf("%T: %s", res.Error, truncate(res.Error.Error(), maxLogLen))
	}
	return out
}

// describeMsgs summarizes a tx's messages as "type path.Func" strings.
func describeMsgs(txBytes bft.Tx) []string {
	var tx std.Tx
	if err := amino.Unmarshal(txBytes, &tx); err != nil {
		return []string{"undecodable: " + err.Error()}
	}
	out := make([]string, len(tx.Msgs))
	for i, msg := range tx.Msgs {
		desc := msg.Route() + "/" + msg.Type()
		switch m := msg.(type) {
		case vm.MsgCall:
			desc += " " + m.PkgPath + "." + m.Func
		case vm.MsgAddPackage:
			if m.Package != nil {
				desc += " " + m.Package.Path
			}
		case vm.MsgRun:
			desc += " " + m.Caller.String()
		}
		out[i] = desc
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Summary is a short human-readable account of the report.
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "replayed %s heights %d..%d: %d blocks, %d txs in %.0fs (genesis %.0fs)\n",
		r.ChainID, r.FromHeight, r.ToHeight, r.Blocks, r.Txs, r.Timing.TotalSeconds, r.Timing.GenesisSeconds)
	fmt.Fprintf(&b, "diffs: %d result, %d gas-only, %d block, %d log-only\n",
		r.Counts.Result, r.Counts.Gas, r.Counts.Block, r.Counts.Log)
	if r.FirstAppHashMismatch > 0 {
		fmt.Fprintf(&b, "first app hash mismatch at height %d\n", r.FirstAppHashMismatch)
	}
	if r.MissingResponses > 0 {
		fmt.Fprintf(&b, "%d heights had no recorded responses to compare\n", r.MissingResponses)
	}
	return b.String()
}
