package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rerendering compares a job with the baseline it had, not one that finished
// while it ran: a job that ran without a baseline keeps none, and a job from
// before base_id was recorded gets the baseline of its start.
func TestRerenderKeepsTheBaselineItHad(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	reports := filepath.Join(dataDir, "reports")
	require.NoError(t, os.MkdirAll(reports, 0o755))
	write := func(name, b string) string {
		p := filepath.Join(root, name)
		require.NoError(t, os.WriteFile(p, []byte(b), 0o644))
		return p
	}
	golden := filepath.Join(root, "golden")
	require.NoError(t, os.MkdirAll(golden, 0o755))
	overlay := filepath.Join(root, "overlay")
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	cfgPath := write("config.toml", `
data_dir = "`+dataDir+`"
[github]
token_file = "`+write("gh-token", "t")+`"
[chain]
golden_dir = "`+golden+`"
genesis = "`+write("genesis.json", "{}")+`"
[job]
gnoreplay_overlay = "`+overlay+`"
`)

	// A report whose one diff is at diffHeight.
	report := func(diffHeight int64) string {
		r := &Report{ChainID: "gnoland-1", ToHeight: 50, Blocks: 50, Txs: 5, Diffs: []Diff{
			{Kind: "result", Height: diffHeight, Index: 0, Recorded: &Result{}, Replayed: &Result{Error: "boom"}},
		}}
		bz, err := json.Marshal(r)
		require.NoError(t, err)
		p := filepath.Join(reports, fmt.Sprintf("%d.json", diffHeight))
		require.NoError(t, os.WriteFile(p, bz, 0o644))
		return p
	}
	path10, path20 := report(10), report(20)

	q, err := openQueue(filepath.Join(dataDir, "jobs.db"))
	require.NoError(t, err)

	// insert writes a finished job with the given times, as the queue would.
	insert := func(event, branch string, pr int, sha string, started, finished time.Time, reportPath, outcome, summary string, baseID int64) int64 {
		t.Helper()
		res, err := q.db.Exec(`INSERT INTO jobs (key, repo, event, branch, pr, sha, priority, state, enqueued_at, started_at, finished_at, report_path, outcome, summary, base_id)
			VALUES (?, ?, ?, ?, ?, ?, 3, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sha, "gnolang/gno", event, branch, pr, sha, stateDone,
			started.UnixNano(), started.UnixNano(), finished.UnixNano(), reportPath, outcome, summary, baseID)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	t0 := time.Now().Add(-3 * time.Hour)
	t1 := time.Now().Add(-2 * time.Hour) // a baseline's replay finishing mid-run
	t2 := time.Now().Add(-1 * time.Hour)

	// A PR job into release that ran with no baseline: release's replay
	// finished while it ran, so rerendering must not now compare it and turn
	// it into a pass.
	insert(eventPush, "release", 0, "bbb", t1, t1, path10, outcomePass, "same", 0)
	ranAlone := insert(eventPullRequest, "release", 7, "aaa", t0, t2, path10,
		outcomeDiverges, "1 new divergence(s) from gnoland-1 history", 0)

	// A job from before base_id was recorded: its start's baseline was B0,
	// though B1 finished during the run.
	base0 := insert(eventPush, "master", 0, "b0", t0.Add(-30*time.Minute), t0.Add(-30*time.Minute), path10, outcomePass, "same", 0)
	insert(eventPush, "master", 0, "b1", t1, t1, path20, outcomePass, "same", 0)
	legacy := insert(eventPullRequest, "master", 8, "c0", t0, t2, path10,
		outcomeDiverges, "1 new divergence(s) from gnoland-1 history", 0)

	require.NoError(t, q.Close())
	require.NoError(t, rerender(cfgPath, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]string{strconv.FormatInt(ranAlone, 10), strconv.FormatInt(legacy, 10)}))

	q, err = openQueue(filepath.Join(dataDir, "jobs.db"))
	require.NoError(t, err)
	defer q.Close()

	got, err := q.Get(ranAlone)
	require.NoError(t, err)
	assert.Equal(t, outcomeDiverges, got.Outcome, "a job with no baseline keeps none")
	assert.Zero(t, got.BaseID)

	got, err = q.Get(legacy)
	require.NoError(t, err)
	assert.Equal(t, base0, got.BaseID, "the baseline of its start, not one from mid-run")
	assert.Equal(t, outcomePass, got.Outcome, "inherited from that baseline")
}
