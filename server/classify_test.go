package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	base := &Report{ToHeight: 100, Diffs: []Diff{
		{Kind: "result", Height: 10, Index: 0}, // still diverges on the PR
		{Kind: "gas", Height: 20, Index: 1},    // fixed by the PR
		{Kind: "block", Height: 10, Index: -1, Detail: "app hash AA"},
	}}
	pr := &Report{ToHeight: 100, Diffs: []Diff{
		{Kind: "result", Height: 10, Index: 0},
		{Kind: "block", Height: 10, Index: -1, Detail: "app hash BB"}, // same divergence, other hash
		{Kind: "result", Height: 50, Index: 2},                        // new
	}}

	c := classify(pr, base)
	assert.False(t, c.NoBaseline)
	assert.False(t, c.Partial)
	assert.Equal(t, []Diff{pr.Diffs[2]}, c.New)
	assert.Equal(t, []Diff{pr.Diffs[0], pr.Diffs[1]}, c.Inherited)
	assert.Equal(t, []Diff{base.Diffs[1]}, c.Fixed)

	// A base diff beyond the PR's replayed range is unchecked, not fixed.
	short := &Report{ToHeight: 15, Diffs: []Diff{{Kind: "result", Height: 10, Index: 0}, {Kind: "block", Height: 10, Index: -1}}}
	c = classify(short, base)
	assert.True(t, c.Partial)
	assert.Empty(t, c.Fixed)
	assert.Empty(t, c.New)

	c = classify(pr, nil)
	assert.True(t, c.NoBaseline)
	assert.Equal(t, pr.Diffs, c.New)
}

func TestRender(t *testing.T) {
	job := &Job{Event: eventPullRequest, Branch: "master"}
	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 100, Blocks: 100, Txs: 5}

	baseJob := &Job{ID: 4, Branch: "master", SHA: "87f0357fe2b373476bb92a26d59833402b9916d3"}
	out := render(job, clean, classify(clean, clean), "", baseJob)
	assert.Equal(t, outcomePass, out.Outcome)
	assert.Contains(t, out.Summary, "replays identically")
	assert.Contains(t, out.Body, "Compared with `master` at commit `87f0357fe` ([its replay](/jobs/4))")

	// Gas-only changes don't count as diverging, but are reported.
	gasOnly := &Report{ChainID: "gnoland-1", ToHeight: 100, Diffs: []Diff{{Kind: "gas", Height: 3, Index: 0, Detail: "gas_used 1 -> 2"}}}
	out = render(job, gasOnly, classify(gasOnly, clean), "", nil)
	assert.Equal(t, outcomePass, out.Outcome)
	assert.Contains(t, out.Summary, "1 tx(s) use different gas")
	assert.Contains(t, out.Body, "gas_used 1 -> 2")
	assert.NotContains(t, out.Body, "Compared with")

	// Divergences and gas-only differences are listed apart: each heading
	// counts what the summary does.
	mixed := &Report{ChainID: "gnoland-1", ToHeight: 100, Diffs: []Diff{
		{Kind: "gas", Height: 2, Index: 0},
		{Kind: "result", Height: 3, Index: 0},
		{Kind: "gas", Height: 4, Index: 0},
		{Kind: "block", Height: 5, Index: -1},
	}}
	out = render(job, mixed, classify(mixed, clean), "", nil)
	assert.Contains(t, out.Summary, "2 new divergence(s)")
	assert.Contains(t, out.Body, "## New divergences (2)")
	assert.Contains(t, out.Body, "## New gas-only differences (2)")

	// Same tx results, other state (e.g. a stdlib change): still diverges.
	appHash := &Report{ChainID: "gnoland-1", ToHeight: 100, FirstAppHashMismatch: 1, Diffs: []Diff{{Kind: "block", Height: 1, Index: -1}}}
	out = render(job, appHash, classify(appHash, clean), "", nil)
	assert.Equal(t, outcomeDiverges, out.Outcome)
	assert.Equal(t, "Every tx result is the same, but the app hash differs from gnoland-1 history", out.Summary)

	broken := &Report{ChainID: "gnoland-1", ToHeight: 100, FirstAppHashMismatch: 7, Diffs: []Diff{
		{Kind: "result", Height: 9, Index: 0, AfterDivergence: true},
		{
			Kind: "result", Height: 7, Index: 1, TxHash: "ABCD", Msgs: []string{"vm/exec gno.land/r/x.F"},
			Recorded: &Result{GasUsed: 10}, Replayed: &Result{Error: "vm.VMError: boom\nstack", GasUsed: 12},
		},
	}}
	out = render(job, broken, classify(broken, clean), "https://replay.example/reports/1", nil)
	assert.Equal(t, outcomeDiverges, out.Outcome)
	assert.Contains(t, out.Summary, "2 new divergence(s) from gnoland-1 history")
	assert.Contains(t, out.Body, "First app hash mismatch at height **7**")
	assert.Contains(t, out.Body, "https://replay.example/reports/1")
	// Root causes are listed before diffs after the divergence.
	assert.Less(t, indexOf(out.Body, "height 7"), indexOf(out.Body, "height 9"))
	assert.Contains(t, out.Body, "`vm.VMError: boom stack` (gas 12)")

	// Inherited-only divergences don't flag the PR.
	out = render(job, broken, classify(broken, broken), "", nil)
	assert.Equal(t, outcomePass, out.Outcome)
	assert.Contains(t, out.Summary, "2 inherited from master")
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
