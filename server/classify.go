package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Report is the subset of gnoreplay's JSON report the server uses.
type Report struct {
	ChainID              string `json:"chain_id"`
	FromHeight           int64  `json:"from_height"`
	ToHeight             int64  `json:"to_height"`
	Blocks               int64  `json:"blocks"`
	Txs                  int64  `json:"txs"`
	FirstAppHashMismatch int64  `json:"first_app_hash_mismatch"`
	Counts               struct {
		Result int `json:"result"`
		Gas    int `json:"gas"`
		Block  int `json:"block"`
		Log    int `json:"log"`
	} `json:"counts"`
	Diffs     []Diff `json:"diffs"`
	Truncated bool   `json:"truncated"`
	Timing    struct {
		TotalSeconds float64 `json:"total_seconds"`
	} `json:"timing"`
}

type Diff struct {
	Kind            string   `json:"kind"`
	Height          int64    `json:"height"`
	Index           int      `json:"index"`
	TxHash          string   `json:"tx_hash"`
	Msgs            []string `json:"msgs"`
	AfterDivergence bool     `json:"after_divergence"`
	Detail          string   `json:"detail"`
	Recorded        *Result  `json:"recorded"`
	Replayed        *Result  `json:"replayed"`
}

type Result struct {
	Error   string `json:"error"`
	Log     string `json:"log"`
	GasUsed int64  `json:"gas_used"`
}

func readReport(path string) (*Report, error) {
	bz, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &Report{}
	return r, json.Unmarshal(bz, r)
}

// id identifies "the same divergence" across two reports. Block diffs carry
// their height only: the detail (e.g. the app hash) differs between binaries.
func (d Diff) id() string {
	if d.Kind == "block" {
		return fmt.Sprintf("block@%d", d.Height)
	}
	return fmt.Sprintf("%s@%d/%d", d.Kind, d.Height, d.Index)
}

// Classification splits a report's diffs against its base branch's report.
type Classification struct {
	New       []Diff // diverges here, not on the base
	Inherited []Diff // diverges on the base too
	Fixed     []Diff // diverges on the base, not here
	// NoBaseline is set when there was no base report: every diff is New.
	NoBaseline bool
	// Partial is set when either report was truncated, or they cover
	// different heights: classification is best-effort.
	Partial bool
}

func classify(r, base *Report) Classification {
	if base == nil {
		return Classification{New: r.Diffs, NoBaseline: true}
	}
	c := Classification{
		Partial: r.Truncated || base.Truncated || r.ToHeight != base.ToHeight,
	}
	baseIDs := make(map[string]bool, len(base.Diffs))
	for _, d := range base.Diffs {
		baseIDs[d.id()] = true
	}
	ids := make(map[string]bool, len(r.Diffs))
	for _, d := range r.Diffs {
		ids[d.id()] = true
		if baseIDs[d.id()] {
			c.Inherited = append(c.Inherited, d)
		} else {
			c.New = append(c.New, d)
		}
	}
	for _, d := range base.Diffs {
		// A base diff past this report's range is not "fixed", just unchecked.
		if !ids[d.id()] && d.Height <= r.ToHeight {
			c.Fixed = append(c.Fixed, d)
		}
	}
	return c
}

// consensus reports whether a diff would fork the chain (gas-only diffs
// would not, on their own).
func (d Diff) consensus() bool { return d.Kind != "gas" }

// Limits from the GitHub checks API.
const (
	maxSummaryLen = 65000
	maxTextLen    = 65000
)

// checkOutput renders a check run's title, summary and text. The check is
// advisory: the conclusion is neutral whenever something new diverges.
func checkOutput(job *Job, r *Report, c Classification, reportURL string) (conclusion, title, summary, text string) {
	newConsensus, newGas := 0, 0
	for _, d := range c.New {
		if d.consensus() {
			newConsensus++
		} else {
			newGas++
		}
	}

	switch {
	case newConsensus > 0:
		conclusion = "neutral"
		title = fmt.Sprintf("%d new divergent result(s) replaying %s", newConsensus, r.ChainID)
	case newGas > 0:
		conclusion = "neutral"
		title = fmt.Sprintf("%d tx(s) now use different gas replaying %s", newGas, r.ChainID)
	default:
		conclusion = "success"
		title = fmt.Sprintf("%s history replays identically (%d blocks, %d txs)", r.ChainID, r.Blocks, r.Txs)
		if len(c.Inherited) > 0 {
			title = fmt.Sprintf("no new divergences replaying %s (%d inherited from %s)", r.ChainID, len(c.Inherited), job.Branch)
		}
	}

	var s strings.Builder
	fmt.Fprintf(&s, "Replayed **%s** heights %d–%d (%d blocks, %d txs) with this commit's binary in %s, comparing each tx against the result recorded on chain.\n\n",
		r.ChainID, r.FromHeight, r.ToHeight, r.Blocks, r.Txs, formatSeconds(r.Timing.TotalSeconds))
	s.WriteString("| | result | gas-only | block |\n|---|---|---|---|\n")
	row := func(name string, ds []Diff) {
		var res, gas, blk int
		for _, d := range ds {
			switch d.Kind {
			case "result":
				res++
			case "gas":
				gas++
			default:
				blk++
			}
		}
		fmt.Fprintf(&s, "| %s | %d | %d | %d |\n", name, res, gas, blk)
	}
	row("**new**", c.New)
	if !c.NoBaseline {
		row("inherited from `"+job.Branch+"`", c.Inherited)
		row("fixed vs `"+job.Branch+"`", c.Fixed)
	}
	s.WriteString("\n")
	if r.FirstAppHashMismatch > 0 {
		fmt.Fprintf(&s, "First app hash mismatch at height **%d**; diffs after it are marked *(after divergence)* and may be consequences of earlier ones.\n\n", r.FirstAppHashMismatch)
	}
	if c.NoBaseline && job.Event == eventPullRequest {
		fmt.Fprintf(&s, "No completed replay of `%s` yet: all diffs are listed as new.\n\n", job.Branch)
	}
	if c.Partial {
		s.WriteString("Classification is partial: the reports were truncated or cover different heights.\n\n")
	}
	if reportURL != "" {
		fmt.Fprintf(&s, "Full report: %s\n\n", reportURL)
	}
	s.WriteString("This check is advisory. Intentional behavior changes are expected to be gated by height or shipped through a coordinated upgrade.\n")

	var t strings.Builder
	writeDiffs(&t, "New", c.New)
	if len(c.Fixed) > 0 {
		writeDiffs(&t, "Fixed", c.Fixed)
	}
	return conclusion, title, truncateStr(s.String(), maxSummaryLen), truncateStr(t.String(), maxTextLen)
}

const maxListedDiffs = 50

func writeDiffs(w *strings.Builder, heading string, ds []Diff) {
	if len(ds) == 0 {
		return
	}
	sorted := append([]Diff(nil), ds...)
	sort.SliceStable(sorted, func(i, j int) bool {
		// Root causes first: consensus diffs before divergence, then by height.
		ai, aj := sorted[i].AfterDivergence, sorted[j].AfterDivergence
		if ai != aj {
			return !ai
		}
		if sorted[i].consensus() != sorted[j].consensus() {
			return sorted[i].consensus()
		}
		return sorted[i].Height < sorted[j].Height
	})
	fmt.Fprintf(w, "## %s (%d)\n\n", heading, len(ds))
	for i, d := range sorted {
		if i == maxListedDiffs {
			fmt.Fprintf(w, "…and %d more (see the full report).\n\n", len(sorted)-i)
			break
		}
		fmt.Fprintf(w, "- **%s** at height %d", d.Kind, d.Height)
		if d.Index >= 0 {
			fmt.Fprintf(w, ", tx %d", d.Index)
		}
		if d.TxHash != "" {
			fmt.Fprintf(w, " `%s`", d.TxHash)
		}
		if d.AfterDivergence {
			w.WriteString(" *(after divergence)*")
		}
		w.WriteString("\n")
		if len(d.Msgs) > 0 {
			fmt.Fprintf(w, "  - msgs: `%s`\n", strings.Join(d.Msgs, "`, `"))
		}
		if d.Detail != "" {
			fmt.Fprintf(w, "  - %s\n", d.Detail)
		}
		if d.Kind == "result" && d.Recorded != nil && d.Replayed != nil {
			fmt.Fprintf(w, "  - recorded: %s\n  - replayed: %s\n", describeResult(d.Recorded), describeResult(d.Replayed))
		}
	}
	w.WriteString("\n")
}

func describeResult(r *Result) string {
	if r.Error == "" {
		return fmt.Sprintf("ok (gas %d)", r.GasUsed)
	}
	return fmt.Sprintf("`%s` (gas %d)", truncateStr(oneLine(r.Error), 300), r.GasUsed)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func formatSeconds(s float64) string {
	if s < 120 {
		return fmt.Sprintf("%.0fs", s)
	}
	return fmt.Sprintf("%.0fm", s/60)
}
