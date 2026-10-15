package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Report text comes from the code under test: a PR can make it contain
// markdown, which must not become a link or an image on a page.
func TestJobBodyEscapesUntrustedReportText(t *testing.T) {
	t.Run("failed job", func(t *testing.T) {
		// A failed job's error is the tail of the build or replay output,
		// which can quote the PR's own source.
		body, err := jobBody(&Job{
			State: stateFailed,
			Error: "go build: syntax error\n```\n[click](https://evil.example/)\n```\n![p](https://evil.example/x.png)",
		})
		require.NoError(t, err)
		assert.NotContains(t, string(body), `<a href="https://evil.example/`)
		assert.NotContains(t, string(body), `<img src="https://evil.example/`)
	})

	t.Run("done job", func(t *testing.T) {
		for name, diff := range map[string]Diff{
			"block detail": {
				Kind: "block", Height: 3, Index: -1,
				Detail: "block not applied: panic: boom\n[click](https://evil.example/)\n![p](https://evil.example/x.png)",
			},
			"result error": {Kind: "result", Height: 3, Index: 0, Recorded: &Result{}, Replayed: &Result{
				Error: "boom` ![p](https://evil.example/x.png) `",
			}},
		} {
			r := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 5, Blocks: 5, Txs: 3, Diffs: []Diff{diff}}
			out := render(&Job{Event: eventPush, Branch: "master"}, r, classify(r, nil), "", nil)

			// Stored and served as /jobs/<id> does.
			path := filepath.Join(t.TempDir(), "1.json")
			require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
			require.NoError(t, os.WriteFile(bodyPath(path), []byte(out.Body), 0o644))
			body, err := jobBody(&Job{State: stateDone, ReportPath: path})
			require.NoError(t, err)
			assert.NotContains(t, string(body), `<a href="https://evil.example/`, name)
			assert.NotContains(t, string(body), `<img src="https://evil.example/`, name)
		}
	})
}

// The limit of a listing query must count only the repos the request may
// see: private jobs must not fill it and hide public results.
func TestRecentFilteredByVisibleRepos(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir(), Repos: map[string]RepoConfig{
		"gnolang/gno":       {PublicReports: true},
		"gnolang/gno-fixes": {},
	}}
	cfg.setDefaults()
	q := newTestQueue(t)
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	finish := func(repo, sha, summary string, at time.Time) {
		t.Helper()
		j := &Job{
			Key: jobKey(repo, eventPush, "develop", 0), Repo: repo, Event: eventPush, Branch: "develop",
			SHA: sha, Priority: 4, EnqueuedAt: at,
		}
		superseded, err := q.Enqueue(j)
		require.NoError(t, err)
		require.Empty(t, superseded)
		claimed, err := q.Claim()
		require.NoError(t, err)
		require.NotNil(t, claimed)
		ok, err := q.Finish(claimed.ID, Job{State: stateDone, Outcome: outcomePass, Summary: summary})
		require.NoError(t, err)
		require.True(t, ok)
	}

	// The public result first, then a burst of newer private ones: without
	// repo filtering they fill Recent's limit and hide it.
	finish("gnolang/gno", "aaa", "the public result", time.Now().Add(-2*time.Hour))
	for i := 0; i < recentSize; i++ {
		finish("gnolang/gno-fixes", "p"+string(rune('a'+i%26))+string(rune('0'+i/26)), "a private result", time.Now().Add(-time.Duration(recentSize-i)*time.Minute))
	}

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "the public result", "the public result is in the newest 50 rows of the repos the request may see")
	assert.NotContains(t, rec.Body.String(), "a private result")
}
