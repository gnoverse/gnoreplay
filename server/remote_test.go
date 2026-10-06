package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMachines "boots" a machine by running its user data with bash, as a
// droplet's cloud-init would, and deletes it by killing the script.
type fakeMachines struct {
	dir string

	mu      sync.Mutex
	nextID  int
	running map[string]*fakeMachine // by ID
	created []string                // names
	deleted []string                // names
}

type fakeMachine struct {
	name   string
	cancel context.CancelFunc
	done   chan struct{}
}

func newFakeMachines(dir string) *fakeMachines {
	return &fakeMachines{dir: dir, running: map[string]*fakeMachine{}}
}

func (f *fakeMachines) Create(_ context.Context, name, userData string) (Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	script := filepath.Join(f.dir, name+".sh")
	if err := os.WriteFile(script, []byte(userData), 0o755); err != nil {
		return Machine{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &fakeMachine{name: name, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		exec.CommandContext(ctx, "bash", script).Run()
	}()
	f.running[id] = m
	f.created = append(f.created, name)
	return Machine{ID: id, Name: name}, nil
}

// orphan registers a machine no job owns, as if left by a crash.
func (f *fakeMachines) orphan(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	done := make(chan struct{})
	close(done)
	f.running[strconv.Itoa(f.nextID)] = &fakeMachine{name: name, cancel: func() {}, done: done}
}

func (f *fakeMachines) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	m, ok := f.running[id]
	delete(f.running, id)
	f.mu.Unlock()
	if !ok {
		return nil
	}
	m.cancel()
	<-m.done
	f.mu.Lock()
	f.deleted = append(f.deleted, m.name)
	f.mu.Unlock()
	return nil
}

func (f *fakeMachines) List(context.Context) ([]Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Machine
	for id, m := range f.running {
		out = append(out, Machine{ID: id, Name: m.name})
	}
	return out, nil
}

func (f *fakeMachines) names() (created, deleted []string, running int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...), append([]string(nil), f.deleted...), len(f.running)
}

func TestRemoteReplay(t *testing.T) {
	for _, tool := range []string{"git", "bash", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	root := t.TempDir()

	remote := filepath.Join(root, "remote", "gnolang", "gno.git")
	require.NoError(t, os.MkdirAll(remote, 0o755))
	git(t, remote, "init", "-q")
	git(t, remote, "config", "uploadpack.allowAnySHA1InWant", "true")

	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, Diffs: []Diff{}}
	shaClean := commitReport(t, remote, clean, 0)
	shaCrash := commitReport(t, remote, clean, 1) // the tool exits 1
	require.NoError(t, os.WriteFile(filepath.Join(remote, "fake-sleep"), []byte("1h"), 0o644))
	shaSlow := commitReport(t, remote, clean, 0)

	golden := filepath.Join(root, "golden")
	for _, name := range []string{"blockstore.db", "state.db"} {
		require.NoError(t, os.MkdirAll(filepath.Join(golden, "db", name), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(golden, "db", name, "MARKER"), nil, 0o644))
	}
	genesis := filepath.Join(root, "genesis.json")
	require.NoError(t, os.WriteFile(genesis, []byte(`{"chain_id":"gnoland-1"}`), 0o644))
	overlay := filepath.Join(root, "overlay")
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "go.mod"), []byte("module fake\n\ngo 1.22\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "main.go"), []byte(fakeTool), 0o644))

	cfg := &Config{
		DataDir:      filepath.Join(root, "data"),
		Chain:        ChainConfig{GoldenDir: golden, Genesis: genesis},
		GitHub:       GitHubConfig{GitURL: "file://" + filepath.Join(root, "remote") + "/"},
		Repos:        map[string]RepoConfig{"gnolang/gno": {PublicReports: true}},
		Job:          JobConfig{GnoreplayOverlay: overlay},
		DigitalOcean: &DigitalOceanConfig{},
	}
	cfg.setDefaults()
	q, err := openQueue(filepath.Join(t.TempDir(), "jobs.db"))
	require.NoError(t, err)
	defer q.Close()
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	machines := newFakeMachines(t.TempDir())
	srv.prov = machines
	workerWorkDir = filepath.Join(root, "workers")
	workerSrv := httptest.NewServer(srv.workerRoutes())
	defer workerSrv.Close()
	cfg.DigitalOcean.URL = workerSrv.URL

	ctx := context.Background()
	run := func(sha string) *Job {
		t.Helper()
		require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, sha))
		j, err := q.Claim()
		require.NoError(t, err)
		srv.runJob(ctx, j)
		done, err := q.Get(j.ID)
		require.NoError(t, err)
		return done
	}

	// A replay on a worker machine, which is deleted once it reported.
	ok := run(shaClean)
	require.Equal(t, stateDone, ok.State, ok.Error)
	assert.Equal(t, outcomePass, ok.Outcome)
	report, err := readReport(ok.ReportPath)
	require.NoError(t, err)
	assert.EqualValues(t, 50, report.Blocks)
	created, deleted, running := machines.names()
	assert.Equal(t, []string{machineName(ok.ID)}, created)
	assert.Equal(t, []string{machineName(ok.ID)}, deleted)
	assert.Zero(t, running)
	// The session is gone: its token no longer works.
	assert.Empty(t, srv.sessions)

	// The worker fails: the job fails with the worker's log, and the machine
	// is deleted all the same.
	failed := run(shaCrash)
	assert.Equal(t, stateFailed, failed.State)
	assert.Contains(t, failed.Error, "gnoreplay exited with 1")
	assert.Contains(t, failed.Error, "==> replay")
	_, deleted, running = machines.names()
	assert.Contains(t, deleted, machineName(failed.ID))
	assert.Zero(t, running)

	// A slow replay superseded by a new push: its machine is deleted. The
	// reaper meanwhile spares it, but not a machine no job owns.
	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, shaSlow))
	slow, err := q.Claim()
	require.NoError(t, err)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		srv.runJob(ctx, slow)
	}()
	require.Eventually(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.sessions[slow.ID] != nil
	}, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { _, _, n := machines.names(); return n == 1 }, 30*time.Second, 50*time.Millisecond)

	machines.orphan("gnoreplay-job-999")
	require.NoError(t, srv.reap(ctx))
	_, deleted, running = machines.names()
	assert.Contains(t, deleted, "gnoreplay-job-999")
	assert.NotContains(t, deleted, machineName(slow.ID), "the running job's machine is spared")
	assert.Equal(t, 1, running)

	// Worker endpoints need the job's token.
	for _, token := range []string{"", "wrong"} {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/worker/%d/source", workerSrv.URL, slow.ID), nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		res.Body.Close()
		assert.Equal(t, http.StatusNotFound, res.StatusCode, "token %q", token)
	}

	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, shaClean))
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("superseded job did not stop")
	}
	_, deleted, running = machines.names()
	assert.Contains(t, deleted, machineName(slow.ID))
	assert.Zero(t, running)
	got, err := q.Get(slow.ID)
	require.NoError(t, err)
	assert.Equal(t, stateSuperseded, got.State)
}

func TestWorkerScriptQuoting(t *testing.T) {
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
}
