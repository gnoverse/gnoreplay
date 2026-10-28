package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The oldest reports past reportsMaxSize are deleted, except each branch's
// latest push replay; their pages remain. Reports saved before reports were
// gzipped are gzipped, if kept.
func TestPruneReports(t *testing.T) {
	defer func(old int64) { reportsMaxSize = old }(reportsMaxSize)
	reportsMaxSize = 1000

	reports := filepath.Join(t.TempDir(), "reports")
	require.NoError(t, os.MkdirAll(reports, 0o755))
	q := newTestQueue(t)
	cfg := &Config{DataDir: filepath.Dir(reports), Repos: map[string]RepoConfig{"gnolang/gno": {PublicReports: true}}}
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// gzipped writes a stored report of size bytes; legacy one saved before
	// reports were gzipped, far bigger than the budget but compressible.
	gzipped := func(name string, size int) string {
		p := filepath.Join(reports, name+".json.gz")
		require.NoError(t, os.WriteFile(p, []byte(strings.Repeat("x", size)), 0o600))
		return p
	}
	legacyReport := &Report{ChainID: "gnoland-1", Diffs: make([]Diff, 200)}
	legacy := func(name string) string {
		bz, err := json.Marshal(legacyReport)
		require.NoError(t, err)
		require.Greater(t, len(bz), 2*int(reportsMaxSize))
		p := filepath.Join(reports, name+".json")
		require.NoError(t, os.WriteFile(p, bz, 0o600))
		return p
	}
	t0 := time.Now().Add(-time.Hour)
	insert := func(event, branch, reportPath string) int64 {
		t.Helper()
		t0 = t0.Add(time.Minute)
		require.NoError(t, os.WriteFile(bodyPath(reportPath), []byte("Page of "+filepath.Base(reportPath)), 0o644))
		res, err := q.db.Exec(`INSERT INTO jobs (key, repo, event, branch, sha, priority, state, enqueued_at, finished_at, report_path, outcome)
			VALUES ('k', 'gnolang/gno', ?, ?, 'abc', 3, ?, ?, ?, ?, ?)`,
			event, branch, stateDone, t0.UnixNano(), t0.UnixNano(), reportPath, outcomePass)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}

	// Oldest first.
	oldLegacy := insert(eventPullRequest, "master", legacy("old-legacy"))
	oldMaster := insert(eventPush, "master", gzipped("old-master", 300))
	mainnet := insert(eventPush, "chain/mainnet", gzipped("mainnet", 300)) // its branch's latest
	oldPR := insert(eventPullRequest, "master", gzipped("old-pr", 300))
	newLegacy := insert(eventPullRequest, "master", legacy("new-legacy"))
	master := insert(eventPush, "master", gzipped("master", 300))
	pr := insert(eventPullRequest, "master", gzipped("pr", 300))

	srv.pruneReports(context.Background())
	srv.pruneReports(context.Background()) // nothing more to do

	kept := func(id int64) bool {
		t.Helper()
		j, err := q.Get(id)
		require.NoError(t, err)
		assert.FileExists(t, bodyPath(j.ReportPath), "job %d keeps its page", id)
		return fileExists(j.ReportPath)
	}
	assert.True(t, kept(pr))
	assert.True(t, kept(master))
	assert.True(t, kept(newLegacy), "gzipped, it fits")
	assert.False(t, kept(oldPR), "past the budget")
	assert.True(t, kept(mainnet), "chain/mainnet's latest push, the baseline of its next replays")
	assert.False(t, kept(oldMaster), "past the budget, and not master's latest")
	assert.False(t, kept(oldLegacy), "past the budget, deleted as is")

	j, err := q.Get(newLegacy)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(reports, "new-legacy.json.gz"), j.ReportPath)
	assert.NoFileExists(t, filepath.Join(reports, "new-legacy.json"))
	got, err := readReport(j.ReportPath)
	require.NoError(t, err)
	assert.Equal(t, legacyReport, got)
	assert.NoFileExists(t, filepath.Join(reports, "old-legacy.json"))
	assert.NoFileExists(t, filepath.Join(reports, "old-legacy.json.gz"))
	tmp, err := filepath.Glob(filepath.Join(reports, "*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, tmp)

	// A pruned report's link says what became of it.
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/reports/%d", oldPR), nil))
	assert.Equal(t, http.StatusGone, rec.Code)
	assert.Contains(t, rec.Body.String(), "deleted to save space")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/jobs/%d", oldPR), nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Page of old-pr.json.gz")
}
