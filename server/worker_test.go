package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The overlay is usually a symlink (deploy/README.md links it into the
// gnoreplay checkout).
func TestCopyDirSymlinkedRoot(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(real, "sub", "x.go"), []byte("package sub"), 0o644))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, link))

	dst := filepath.Join(root, "dst")
	require.NoError(t, copyDir(link, dst))
	bz, err := os.ReadFile(filepath.Join(dst, "main.go"))
	require.NoError(t, err)
	assert.Equal(t, "package main", string(bz))
	assert.FileExists(t, filepath.Join(dst, "sub", "x.go"))
}

// Jobs start together on a fresh server (e.g. both branch baselines on the
// first poll): their checkouts share one mirror that does not exist yet.
func TestConcurrentCheckouts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote", "gnolang", "gno.git")
	require.NoError(t, os.MkdirAll(remote, 0o755))
	git(t, remote, "init", "-q")
	git(t, remote, "config", "uploadpack.allowAnySHA1InWant", "true")
	var shas []string
	for i := range 4 {
		require.NoError(t, os.WriteFile(filepath.Join(remote, "f"), fmt.Appendf(nil, "%d", i), 0o644))
		git(t, remote, "add", ".")
		git(t, remote, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
		shas = append(shas, git(t, remote, "rev-parse", "HEAD"))
	}

	cfg := &Config{DataDir: filepath.Join(root, "data"), GitHub: GitHubConfig{GitURL: "file://" + filepath.Join(root, "remote") + "/"}}
	cfg.setDefaults()
	srv := newServer(cfg, nil, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	var wg sync.WaitGroup
	errs := make([]error, len(shas))
	for i, sha := range shas {
		wg.Go(func() {
			parents, err := srv.checkout(context.Background(), "gnolang/gno", sha, filepath.Join(root, "src", fmt.Sprint(i)), io.Discard)
			if err == nil && i > 0 && (len(parents) != 1 || parents[0] != shas[i-1]) {
				err = fmt.Errorf("parents %v, want [%s]", parents, shas[i-1])
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "checkout %d", i)
		bz, err := os.ReadFile(filepath.Join(root, "src", fmt.Sprint(i), "f"))
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprint(i), string(bz))
	}
}

// A sandbox prefix's placeholders: {job}, {src}, and {jobdir}/{data} for
// mounting the job's own paths, never the whole data dir.
func TestSandboxSubstitution(t *testing.T) {
	cfg := &Config{DataDir: "/data"}
	srv := newServer(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got := srv.sandbox(
		[]string{"run", "--mount-dir", "{jobdir}:{jobdir}", "-e", "GOMODCACHE={data}/gomod", "{src}"},
		&Job{ID: 7}, "/data/jobs/7/src")
	assert.Equal(t, []string{
		"run", "--mount-dir", "/data/jobs/7:/data/jobs/7", "-e", "GOMODCACHE=/data/gomod", "/data/jobs/7/src",
	}, got)
}

// The local replay resolves the golden symlink once, before both DB copies: a
// refresh landing between them must not mix two snapshots (the worker path
// resolves once per request for the same reason).
func TestLocalGoldenCopyPinned(t *testing.T) {
	root := t.TempDir()

	// Two snapshots and the golden symlink, as fetch-golden.sh keeps them.
	for _, snap := range []string{"golden-A", "golden-B"} {
		for _, name := range []string{"blockstore.db", "state.db"} {
			p := filepath.Join(root, snap, "db", name)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(snap), 0o644))
		}
	}
	golden := filepath.Join(root, "golden")
	require.NoError(t, os.Symlink(filepath.Join(root, "golden-A"), golden))

	// A copy command that swaps the symlink between the two copies, as a
	// refresh landing in that gap does, and records what each copy read.
	log := filepath.Join(root, "copies.log")
	script := filepath.Join(root, "swap-copy.sh")
	require.NoError(t, os.WriteFile(script, fmt.Appendf(nil, `#!/bin/bash
set -e
src=$1 dst=$2
if [ "$(basename "$src")" = state.db ] && ! grep -q swapped %[1]s; then
	echo swapped >> %[1]s
	ln -sfn %[2]s %[3]s.tmp
	mv -T %[3]s.tmp %[3]s
fi
echo "$(basename "$src") from $(readlink -f "$src")" >> %[1]s
cp -a "$src" "$dst"
`, log, filepath.Join(root, "golden-B"), golden), 0o755))

	// A remote with one commit to replay.
	remote := filepath.Join(root, "remote", "gnolang", "gno.git")
	require.NoError(t, os.MkdirAll(remote, 0o755))
	git(t, remote, "init", "-q")
	git(t, remote, "config", "uploadpack.allowAnySHA1InWant", "true")
	require.NoError(t, os.WriteFile(filepath.Join(remote, "f"), []byte("x"), 0o644))
	git(t, remote, "add", ".")
	git(t, remote, "commit", "-q", "-m", "c")
	sha := git(t, remote, "rev-parse", "HEAD")

	// A stand-in tool reporting which snapshot each DB came from.
	overlay := filepath.Join(root, "overlay")
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "go.mod"), []byte("module fake\n\ngo 1.22\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "main.go"), []byte(`package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	dataDir := flag.String("data-dir", "", "")
	flag.String("genesis", "", "")
	flag.String("work-dir", "", "")
	flag.String("max-diffs", "", "")
	flag.String("log-level", "", "")
	out := flag.String("out", "", "")
	flag.Parse()
	read := func(name string) string {
		bz, _ := os.ReadFile(filepath.Join(*dataDir, "db", name))
		return string(bz)
	}
	report := map[string]any{
		"chain_id": "gnoland-1", "to_height": 1, "diffs": []map[string]any{{
			"kind": "block", "height": 1, "index": -1,
			"detail": fmt.Sprintf("blockstore=%s state=%s", read("blockstore.db"), read("state.db")),
		}},
	}
	bz, _ := json.Marshal(report)
	os.WriteFile(*out, bz, 0o644)
	os.Exit(2)
}
`), 0o644))

	cfg := &Config{
		DataDir: filepath.Join(root, "data"),
		Chain:   ChainConfig{GoldenDir: golden, Genesis: filepath.Join(root, "genesis.json")},
		GitHub:  GitHubConfig{GitURL: "file://" + filepath.Join(root, "remote") + "/"},
		Job: JobConfig{
			GnoreplayOverlay: overlay,
			CopyCommand:      []string{"bash", script, "{src}", "{dst}"},
		},
	}
	cfg.setDefaults()
	require.NoError(t, os.WriteFile(cfg.Chain.Genesis, []byte("{}"), 0o644))
	q := newTestQueue(t)
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, sha))
	j, err := q.Claim()
	require.NoError(t, err)
	srv.runJob(context.Background(), j)
	done, err := q.Get(j.ID)
	require.NoError(t, err)
	require.Equal(t, stateDone, done.State, done.Error)

	report, err := readReport(done.ReportPath)
	require.NoError(t, err)
	require.Len(t, report.Diffs, 1)
	bz, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Contains(t, string(bz), "swapped", "the copy command simulated a refresh between the copies")
	assert.Contains(t, report.Diffs[0].Detail, "blockstore=golden-A state=golden-A", "both DBs come from one snapshot")
}

// The replay flags come from one list, shared by the local runner and the
// worker script's ReplayArgs, so a flag cannot be added to one runner and not
// the other (see worker.sh).
func TestReplayToolArgs(t *testing.T) {
	assert.Equal(t,
		[]string{"--data-dir", "/d", "--genesis", "/g.json", "--work-dir", "/w", "--max-diffs", "0", "--out", "/r.json", "--log-level", "warn"},
		replayToolArgs("/d", "/g.json", "/w", "/r.json", "", "warn"))
	worker := replayToolArgs("data", "genesis.json", "work", "report.json", "progress.json", "")
	assert.Equal(t,
		[]string{"--data-dir", "data", "--genesis", "genesis.json", "--work-dir", "work", "--max-diffs", "0", "--out", "report.json", "--progress", "progress.json"},
		worker)

	var b strings.Builder
	require.NoError(t, workerTmpl.Execute(&b, map[string]string{
		"URL": "u", "JobID": "1", "Token": "t", "WorkDir": "/w",
		"GoVersion": "1", "GoMemLimit": "1GiB", "PowerOffMinutes": "0", "ProgressSeconds": "60",
		"ReplayArgs": shellJoin(worker),
	}))
	assert.Contains(t, b.String(), `./gnoreplay '--data-dir' 'data'`)
}
