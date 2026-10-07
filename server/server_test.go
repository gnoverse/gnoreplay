package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGitHub serves branch heads and open PRs from memory. The GitHub
// interface has no write methods: nothing goes back to GitHub.
type fakeGitHub struct {
	mu       sync.Mutex
	branches map[string]string // "repo@branch" -> sha
	pulls    map[string][]PullRequest
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{branches: map[string]string{}, pulls: map[string][]PullRequest{}}
}

func (f *fakeGitHub) BranchHead(_ context.Context, repo, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha, ok := f.branches[repo+"@"+branch]
	if !ok {
		return "", fmt.Errorf("no branch %s", branch)
	}
	return sha, nil
}

func (f *fakeGitHub) OpenPulls(_ context.Context, repo string) ([]PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PullRequest(nil), f.pulls[repo]...), nil
}

func (f *fakeGitHub) Pull(_ context.Context, repo string, number int) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pr := range f.pulls[repo] {
		if pr.Number == number {
			return pr, nil
		}
	}
	return PullRequest{}, fmt.Errorf("no PR %d", number)
}

func (f *fakeGitHub) Token() string { return "token" }

func (f *fakeGitHub) setPulls(repo string, prs ...PullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[repo] = prs
}

// fakeTool stands in for gnoreplay/: it checks it was given a copy of the
// chain data and a sandbox prefix, then outputs the report the commit under
// test carries (in fake-report.json, with the exit code in fake-exit).
const fakeTool = `package main

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
	root := os.Getenv("GNOROOT")
	if root == "" {
		panic("GNOROOT not set")
	}
	// Run through a local sandbox prefix (env SANDBOX_JOB={job} ...).
	if job, ok := os.LookupEnv("SANDBOX_JOB"); ok && (job == "{job}" || os.Getenv("SANDBOX_SRC") != root) {
		panic("{job} or {src} not substituted in the sandbox prefix")
	}
	if bz, err := os.ReadFile(filepath.Join(root, "fake-sleep")); err == nil {
		d, _ := time.ParseDuration(strings.TrimSpace(string(bz)))
		time.Sleep(d)
	}
	// Wait until the test creates the file named in fake-wait.
	if bz, err := os.ReadFile(filepath.Join(root, "fake-wait")); err == nil {
		for {
			if _, err := os.Stat(strings.TrimSpace(string(bz))); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	report, err := os.ReadFile(filepath.Join(root, "fake-report.json"))
	if err != nil {
		panic(err)
	}
	code, _ := os.ReadFile(filepath.Join(root, "fake-exit"))
	exit, _ := strconv.Atoi(strings.TrimSpace(string(code)))
	os.WriteFile(*out, report, 0o644)
	os.Exit(exit)
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

var commits int

// commitReport commits a tree whose replay yields report (with exit code).
func commitReport(t *testing.T, repo string, report *Report, exit int) string {
	t.Helper()
	bz, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "fake-report.json"), bz, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "fake-exit"), fmt.Appendf(nil, "%d", exit), 0o644))
	git(t, repo, "add", ".")
	// A unique message: identical trees committed in the same second in two
	// repos would otherwise get the same SHA.
	commits++
	git(t, repo, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("commit %d", commits))
	return git(t, repo, "rev-parse", "HEAD")
}

func TestServerEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()

	// The "GitHub" remotes: file:///<root>/remote/<owner>/<name>.git
	newRemote := func(name string) string {
		dir := filepath.Join(root, "remote", name+".git")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		git(t, dir, "init", "-q")
		git(t, dir, "config", "uploadpack.allowAnySHA1InWant", "true")
		return dir
	}
	gno, fixes := newRemote("gnolang/gno"), newRemote("gnolang/gno-fixes")

	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, Diffs: []Diff{}}
	broken := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, FirstAppHashMismatch: 12}
	broken.Counts.Result = 1
	broken.Diffs = []Diff{{
		Kind: "result", Height: 12, Index: 0, TxHash: "AB12", Msgs: []string{"vm/exec gno.land/r/demo/foo.Bar"},
		Recorded: &Result{GasUsed: 100}, Replayed: &Result{Error: "vm.VMError: <script>unexpected</script>", GasUsed: 90},
	}}
	shaMaster := commitReport(t, gno, clean, 0)
	shaOldPR := commitReport(t, gno, clean, 0)
	shaPR := commitReport(t, gno, broken, 2)
	shaPR2 := commitReport(t, gno, broken, 2)
	shaFixes := commitReport(t, fixes, clean, 0)
	shaFixesPR := commitReport(t, fixes, broken, 2)

	golden := filepath.Join(root, "golden")
	for _, name := range []string{"blockstore.db", "state.db"} {
		require.NoError(t, os.MkdirAll(filepath.Join(golden, "db", name), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(golden, "db", name, "MARKER"), nil, 0o644))
	}
	overlay := filepath.Join(root, "overlay")
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "go.mod"), []byte("module fake\n\ngo 1.22\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "main.go"), []byte(fakeTool), 0o644))

	cfg := &Config{
		DataDir: filepath.Join(root, "data"),
		// Fake tool, fake chain: nothing but paths.
		Chain:     ChainConfig{GoldenDir: golden, Genesis: filepath.Join(root, "genesis.json")},
		GitHub:    GitHubConfig{GitURL: "file://" + filepath.Join(root, "remote") + "/"},
		PublicURL: "https://replay.example",
		Repos:     map[string]RepoConfig{"gnolang/gno": {PublicReports: true}},
		Job: JobConfig{
			GnoreplayOverlay: overlay,
			// A stand-in for a VM sandbox: it must get the job and checkout.
			RunSandbox:     []string{"env", "SANDBOX_JOB={job}", "SANDBOX_SRC={src}"},
			CleanupCommand: []string{"touch", filepath.Join(root, "cleaned-{job}")},
		},
	}
	cfg.setDefaults()
	q, err := openQueue(filepath.Join(t.TempDir(), "jobs.db"))
	require.NoError(t, err)
	defer q.Close()
	gh := newFakeGitHub()
	srv := newServer(cfg, q, gh, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.viewerKey = "viewer-key"
	httpSrv := httptest.NewServer(srv.routes())
	defer httpSrv.Close()

	ctx := context.Background()
	poll := func() {
		t.Helper()
		for _, repo := range cfg.repos() {
			require.NoError(t, srv.pollRepo(ctx, repo))
		}
	}
	runNext := func() *Job {
		t.Helper()
		j, err := q.Claim()
		require.NoError(t, err)
		require.NotNil(t, j)
		srv.runJob(ctx, j)
		done, err := q.Get(j.ID)
		require.NoError(t, err)
		return done
	}
	get := func(client *http.Client, path string) (int, string) {
		t.Helper()
		res, err := client.Get(httpSrv.URL + path)
		require.NoError(t, err)
		defer res.Body.Close()
		bz, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, string(bz)
	}
	anon := &http.Client{}

	// First pass: tracked branches are replayed (they are the baselines);
	// PRs already open are only recorded.
	gh.branches["gnolang/gno@chain/mainnet"] = shaMaster
	gh.branches["gnolang/gno@master"] = shaMaster
	gh.branches["gnolang/gno-fixes@develop"] = shaFixes
	gh.setPulls("gnolang/gno",
		PullRequest{Number: 5, Base: "master", HeadSHA: shaOldPR},
		PullRequest{Number: 6, Base: "some-feature", HeadSHA: shaOldPR}, // untracked base
	)
	poll()
	pending, err := q.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 3, "the three tracked branches")

	// Nothing changed: nothing new.
	poll()
	pending, _ = q.Pending()
	assert.Len(t, pending, 3)

	mainnetJob, masterJob, fixesJob := runNext(), runNext(), runNext()
	assert.Equal(t, "gnolang/gno#branch/chain/mainnet", mainnetJob.Key, "priority 1 first")
	assert.Equal(t, "gnolang/gno#branch/master", masterJob.Key)
	assert.Equal(t, "gnolang/gno-fixes#branch/develop", fixesJob.Key)
	assert.Equal(t, outcomePass, masterJob.Outcome)
	assert.Contains(t, masterJob.Summary, "replays identically")

	// A PR is opened, then pushed to before its replay starts: the newer
	// head supersedes the first.
	pr7 := PullRequest{Number: 7, Base: "master", HeadSHA: shaPR, Title: "<b>Add</b> things", Author: "alice"}
	gh.setPulls("gnolang/gno", PullRequest{Number: 5, Base: "master", HeadSHA: shaOldPR}, pr7)
	poll()
	pr7.HeadSHA = shaPR2
	gh.setPulls("gnolang/gno", PullRequest{Number: 5, Base: "master", HeadSHA: shaOldPR}, pr7)
	poll()
	prJob := runNext()
	assert.Equal(t, shaPR2, prJob.SHA)
	assert.Equal(t, outcomeDiverges, prJob.Outcome)
	assert.Contains(t, prJob.Summary, "1 new divergence(s) from gnoland-1 history")
	assert.Equal(t, "<b>Add</b> things", prJob.Title)
	assert.Equal(t, masterJob.ID, prJob.BaseID)

	// Results are on the server: the PR's GitHub path lists its replays, the
	// superseded first push included, and shows the latest in full.
	code, page := get(anon, "/gnolang/gno/pull/7")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, "PR #7")
	assert.Contains(t, page, "&lt;b&gt;Add&lt;/b&gt; things", "PR titles are escaped")
	assert.Contains(t, page, "by @alice")
	assert.Contains(t, page, "https://github.com/gnolang/gno/commit/"+shaPR2)
	assert.Contains(t, page, shaPR[:9])
	assert.Contains(t, page, "not checked: superseded")
	assert.Contains(t, page, "<h2>New divergences (1)</h2>")
	assert.Contains(t, page, "gno.land/r/demo/foo.Bar")
	assert.Contains(t, page, "vm.VMError:")
	assert.NotContains(t, page, "<script>unexpected", "tx errors are not rendered as HTML")
	code, page = get(anon, fmt.Sprintf("/jobs/%d", prJob.ID))
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, fmt.Sprintf(`<a href="/jobs/%d">master at <code>%s</code></a>`, masterJob.ID, shortSHA(masterJob.SHA)),
		"the baseline it was compared with")
	assert.Contains(t, page, `<time datetime="`)
	code, page = get(anon, "/gnolang/gno/tree/chain/mainnet")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, "Branch chain/mainnet")
	code, _ = get(anon, "/gnolang/gno/pull/99")
	assert.Equal(t, http.StatusNotFound, code)
	code, report := get(anon, fmt.Sprintf("/reports/%d", prJob.ID))
	require.Equal(t, http.StatusOK, code)
	var served Report
	require.NoError(t, json.Unmarshal([]byte(report), &served))
	assert.Len(t, served.Diffs, 1)

	code, index := get(anon, "/")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, index, "1 new divergence(s)")
	assert.Contains(t, index, "pushes to gnolang/gno chain/mainnet")
	assert.Contains(t, index, sourceURL)

	// Search leads to a PR's, a branch's or a commit's replays.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for q, want := range map[string]string{
		"7":  "/gnolang/gno/pull/7",
		"#7": "/gnolang/gno/pull/7",
		"https://github.com/gnolang/gno/pull/7/files": "/gnolang/gno/pull/7",
		"chain/mainnet": "/gnolang/gno/tree/chain/mainnet",
		"THINGS":        "/gnolang/gno/pull/7",
		shaPR2[:7]:      fmt.Sprintf("/jobs/%d", prJob.ID),
		"github.com/gnolang/gno/commit/" + shaPR2: fmt.Sprintf("/jobs/%d", prJob.ID),
	} {
		res, err := noRedirect.Get(httpSrv.URL + "/search?q=" + url.QueryEscape(q))
		require.NoError(t, err)
		res.Body.Close()
		assert.Equal(t, http.StatusFound, res.StatusCode, q)
		assert.Equal(t, want, res.Header.Get("Location"), q)
	}
	code, page = get(anon, "/search?q=nothing+like+it")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, "No replay matches")

	// A PR of the private repo: invisible without the viewer key, even
	// queued.
	gh.setPulls("gnolang/gno-fixes", PullRequest{Number: 3, Base: "develop", HeadSHA: shaFixesPR})
	poll()
	_, index = get(anon, "/")
	assert.NotContains(t, index, "gno-fixes")
	assert.Contains(t, index, "a private repository's replay")
	fixesPR := runNext()
	assert.Equal(t, outcomeDiverges, fixesPR.Outcome)
	for _, path := range []string{
		fmt.Sprintf("/jobs/%d", fixesPR.ID), fmt.Sprintf("/reports/%d", fixesPR.ID),
		"/gnolang/gno-fixes/pull/3", fmt.Sprintf("/jobs/%d?key=wrong", fixesPR.ID),
	} {
		code, _ = get(anon, path)
		assert.Equal(t, http.StatusNotFound, code, path)
	}
	_, page = get(anon, "/search?q=3")
	assert.Contains(t, page, "No replay matches")
	_, index = get(anon, "/")
	assert.NotContains(t, index, "gno-fixes")
	_, queue := get(anon, "/queue")
	assert.NotContains(t, queue, "gno-fixes")

	// With the key once, a cookie keeps the private results visible.
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	viewer := &http.Client{Jar: jar}
	code, _ = get(viewer, "/?key=viewer-key")
	require.Equal(t, http.StatusOK, code)
	code, page = get(viewer, "/gnolang/gno-fixes/pull/3")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, page, "PR #3")
	assert.Contains(t, page, "gnolang/gno-fixes · into")
	_, index = get(viewer, "/")
	assert.Contains(t, index, "gnolang/gno-fixes")

	// A PR closed while queued is cancelled.
	gh.setPulls("gnolang/gno",
		PullRequest{Number: 5, Base: "master", HeadSHA: shaOldPR},
		PullRequest{Number: 8, Base: "master", HeadSHA: shaPR},
	)
	poll()
	pending, _ = q.Pending()
	require.Len(t, pending, 1)
	gh.setPulls("gnolang/gno", PullRequest{Number: 5, Base: "master", HeadSHA: shaOldPR})
	poll()
	pending, _ = q.Pending()
	assert.Empty(t, pending)

	// A PR open since before the first pass can be replayed on demand.
	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPullRequest, "master", 5, shaOldPR))
	oldPR := runNext()
	assert.Equal(t, "gnolang/gno#pr/5", oldPR.Key)
	assert.Equal(t, outcomePass, oldPR.Outcome)

	// Job dirs are removed; reports are kept; cleanup ran for every job.
	for _, j := range []*Job{mainnetJob, masterJob, fixesJob, prJob, fixesPR, oldPR} {
		_, err = os.Stat(filepath.Join(cfg.DataDir, "jobs", fmt.Sprint(j.ID)))
		assert.True(t, os.IsNotExist(err))
		assert.FileExists(t, filepath.Join(root, fmt.Sprintf("cleaned-%d", j.ID)))
	}
}

func TestNoViewerKey(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	cfg.setDefaults()
	q := newTestQueue(t)
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, srv.enqueueFor("gnolang/gno-fixes", eventPush, "develop", 0, "abc"))

	// Without a configured key, no key unlocks private results, not even an
	// empty one.
	for _, path := range []string{"/jobs/1", "/jobs/1?key=", "/gnolang/gno-fixes/tree/develop"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
	}
}

func TestPollErrorKeepsHeads(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	cfg.setDefaults()
	cfg.Rules = []Rule{{Repo: "gnolang/gno", Event: eventPush, Branch: "master"}, {Repo: "gnolang/gno", Event: eventPullRequest, Branch: "master"}}
	q := newTestQueue(t)
	gh := newFakeGitHub()
	srv := newServer(cfg, q, gh, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	gh.branches["gnolang/gno@master"] = "aaa"
	gh.setPulls("gnolang/gno", PullRequest{Number: 1, Base: "master", HeadSHA: "bbb"})
	require.NoError(t, srv.pollRepo(ctx, "gnolang/gno"))
	gh.setPulls("gnolang/gno", PullRequest{Number: 1, Base: "master", HeadSHA: "ccc"})
	require.NoError(t, srv.pollRepo(ctx, "gnolang/gno"))

	// The branch lookup fails: nothing may be treated as gone.
	delete(gh.branches, "gnolang/gno@master")
	require.Error(t, srv.pollRepo(ctx, "gnolang/gno"))
	heads, err := q.Heads("gnolang/gno")
	require.NoError(t, err)
	assert.Len(t, heads, 2)
	pending, err := q.Pending()
	require.NoError(t, err)
	assert.Len(t, pending, 2, "the branch and the PR push stay queued")
}
