package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCheck struct {
	repo, sha string
	updates   []checkUpdate
}

type fakeGitHub struct {
	mu     sync.Mutex
	checks []*fakeCheck
}

func (f *fakeGitHub) CreateCheck(_ context.Context, _ int64, repo, sha string, u checkUpdate) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, &fakeCheck{repo: repo, sha: sha, updates: []checkUpdate{u}})
	return int64(len(f.checks)), nil
}

func (f *fakeGitHub) UpdateCheck(_ context.Context, _ int64, _ string, id int64, u checkUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.checks[id-1]
	c.updates = append(c.updates, u)
	return nil
}

func (f *fakeGitHub) Token(context.Context, int64) (string, error) { return "token", nil }

func (f *fakeGitHub) last(sha string) checkUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.checks) - 1; i >= 0; i-- {
		if f.checks[i].sha == sha {
			return f.checks[i].updates[len(f.checks[i].updates)-1]
		}
	}
	return checkUpdate{}
}

// fakeTool is a stand-in contribs/gnoreplay: it checks it was given a copy
// of the chain data, then writes a canned report.
const fakeTool = `package main

import (
	"flag"
	"os"
	"path/filepath"
)

const report = %q

func main() {
	dataDir := flag.String("data-dir", "", "")
	out := flag.String("out", "", "")
	flag.String("genesis", "", "")
	flag.String("work-dir", "", "")
	flag.String("max-diffs", "", "")
	flag.String("log-level", "", "")
	flag.Parse()
	if _, err := os.Stat(filepath.Join(*dataDir, "db", "state.db", "MARKER")); err != nil {
		panic(err)
	}
	if os.Getenv("GNOROOT") == "" || os.Getenv("SANDBOX_SRC") != os.Getenv("GNOROOT") {
		panic("GNOROOT not set, or {src} not substituted in the sandbox prefix")
	}
	if os.Getenv("SANDBOX_JOB") == "" || os.Getenv("SANDBOX_JOB") == "{job}" {
		panic("{job} not substituted in the sandbox prefix")
	}
	os.WriteFile(*out, []byte(report), 0o644)
	os.Exit(%d)
}
`

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// commitTool commits a fake gnoreplay that outputs report (with exit code).
func commitTool(t *testing.T, repo string, report *Report, exit int) string {
	t.Helper()
	bz, err := json.Marshal(report)
	require.NoError(t, err)
	dir := filepath.Join(repo, "contribs", "gnoreplay")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fake\n\ngo 1.22\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), fmt.Appendf(nil, fakeTool, bz, exit), 0o644))
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "tool")
	return git(t, repo, "rev-parse", "HEAD")
}

func TestServerEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()

	// The "GitHub" remote: file:///<root>/remote/gnolang/gno.git
	repo := filepath.Join(root, "remote", "gnolang", "gno.git")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	git(t, repo, "init", "-q")
	git(t, repo, "config", "uploadpack.allowAnySHA1InWant", "true")

	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, Diffs: []Diff{}}
	shaBase := commitTool(t, repo, clean, 0)
	broken := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, FirstAppHashMismatch: 12}
	broken.Counts.Result = 1
	broken.Diffs = []Diff{{
		Kind: "result", Height: 12, Index: 0, TxHash: "AB12", Msgs: []string{"vm/exec gno.land/r/demo/foo.Bar"},
		Recorded: &Result{GasUsed: 100}, Replayed: &Result{Error: "vm.VMError: unexpected", GasUsed: 90},
	}}
	shaPR := commitTool(t, repo, broken, 2)

	golden := filepath.Join(root, "golden")
	for _, name := range []string{"blockstore.db", "state.db"} {
		require.NoError(t, os.MkdirAll(filepath.Join(golden, "db", name), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(golden, "db", name, "MARKER"), nil, 0o644))
	}

	cfg := &Config{
		DataDir: filepath.Join(root, "data"),
		// Fake tool, fake chain: nothing but paths.
		Chain:     ChainConfig{GoldenDir: golden, Genesis: filepath.Join(root, "genesis.json")},
		GitHub:    GitHubConfig{AppID: 1, GitURL: "file://" + filepath.Join(root, "remote") + "/"},
		PublicURL: "https://replay.example",
		Repos:     map[string]RepoConfig{"gnolang/gno": {PublicReports: true}},
		Job: JobConfig{
			// A stand-in for a VM sandbox: it must get the job and checkout.
			RunSandbox:     []string{"env", "SANDBOX_JOB={job}", "SANDBOX_SRC={src}"},
			CleanupCommand: []string{"touch", filepath.Join(root, "cleaned-{job}")},
		},
	}
	cfg.setDefaults()
	q, err := openQueue(filepath.Join(t.TempDir(), "jobs.db"))
	require.NoError(t, err)
	defer q.Close()
	gh := &fakeGitHub{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServer(cfg, q, gh, logger)

	secret := []byte("s3cret")
	httpSrv := httptest.NewServer(srv.routes(secret))
	defer httpSrv.Close()
	deliver := func(event string, payload any, sign bool) int {
		bz, err := json.Marshal(payload)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/webhook", bytes.NewReader(bz))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", event)
		if sign {
			mac := hmac.New(sha256.New, secret)
			mac.Write(bz)
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		res.Body.Close()
		return res.StatusCode
	}
	runNext := func() *Job {
		j, err := q.Claim()
		require.NoError(t, err)
		require.NotNil(t, j)
		srv.runJob(context.Background(), j)
		return j
	}

	push := map[string]any{
		"ref": "refs/heads/master", "after": shaBase,
		"repository":   map[string]any{"full_name": "gnolang/gno"},
		"installation": map[string]any{"id": 42},
	}
	assert.Equal(t, http.StatusUnauthorized, deliver("push", push, false), "unsigned deliveries are rejected")
	require.Equal(t, http.StatusNoContent, deliver("push", push, true))
	assert.Equal(t, "queued", gh.last(shaBase).Status)

	// Not matched by any rule: ignored.
	require.Equal(t, http.StatusNoContent, deliver("push", map[string]any{
		"ref": "refs/heads/feature", "after": shaPR,
		"repository": map[string]any{"full_name": "gnolang/gno"}, "installation": map[string]any{"id": 42},
	}, true))
	assert.Empty(t, gh.last(shaPR).Status)

	baseJob := runNext()
	u := gh.last(shaBase)
	assert.Equal(t, "completed", u.Status)
	assert.Equal(t, "success", u.Conclusion, "%s\n%s", u.Title, u.Summary)
	assert.Contains(t, u.Title, "replays identically")

	require.Equal(t, http.StatusNoContent, deliver("pull_request", map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"number": 7,
			"base":   map[string]any{"ref": "master"},
			"head":   map[string]any{"sha": shaPR},
		},
		"repository":   map[string]any{"full_name": "gnolang/gno"},
		"installation": map[string]any{"id": 42},
	}, true))
	prJob := runNext()
	u = gh.last(shaPR)
	assert.Equal(t, "completed", u.Status)
	assert.Equal(t, "neutral", u.Conclusion, "advisory: divergences are neutral")
	assert.Contains(t, u.Title, "1 new divergent result")
	assert.Contains(t, u.Text, "gno.land/r/demo/foo.Bar")
	assert.Contains(t, u.Text, "vm.VMError: unexpected")
	assert.Equal(t, fmt.Sprintf("https://replay.example/reports/%d", prJob.ID), u.DetailsURL)

	// The full report is served for public repos.
	res, err := http.Get(fmt.Sprintf("%s/reports/%d", httpSrv.URL, prJob.ID))
	require.NoError(t, err)
	var served Report
	require.NoError(t, json.NewDecoder(res.Body).Decode(&served))
	res.Body.Close()
	assert.Len(t, served.Diffs, 1)

	// Job dirs are removed; reports are kept.
	_, err = os.Stat(filepath.Join(cfg.DataDir, "jobs", fmt.Sprint(baseJob.ID)))
	assert.True(t, os.IsNotExist(err))
	// The cleanup command ran for each job.
	for _, j := range []*Job{baseJob, prJob} {
		assert.FileExists(t, filepath.Join(root, fmt.Sprintf("cleaned-%d", j.ID)))
	}
}
