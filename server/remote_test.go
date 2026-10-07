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
	"strings"
	"sync"
	"sync/atomic"
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
	name    string
	created time.Time
	cancel  context.CancelFunc
	done    chan struct{}
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
	m := &fakeMachine{name: name, created: time.Now(), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		exec.CommandContext(ctx, "bash", script).Run()
	}()
	f.running[id] = m
	f.created = append(f.created, name)
	return Machine{ID: id, Name: name, Created: m.created}, nil
}

// orphan registers a machine no job owns, as if left by a crash.
func (f *fakeMachines) orphan(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	done := make(chan struct{})
	close(done)
	f.running[strconv.Itoa(f.nextID)] = &fakeMachine{name: name, created: time.Now(), cancel: func() {}, done: done}
}

// age makes a machine look created d ago.
func (f *fakeMachines) age(name string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.running {
		if m.name == name {
			m.created = time.Now().Add(-d)
		}
	}
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
		out = append(out, Machine{ID: id, Name: m.name, Created: m.created})
	}
	return out, nil
}

func (f *fakeMachines) names() (created, deleted []string, running int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...), append([]string(nil), f.deleted...), len(f.running)
}

// remoteEnv runs remote replays on fakeMachines, with a worker endpoint
// that a restarted server takes over.
type remoteEnv struct {
	root, remote string // remote: the gnolang/gno repo
	cfg          *Config
	q            *Queue
	machines     *fakeMachines
	srv          *Server
	handler      atomic.Value // the worker endpoint's http.Handler
}

func newRemoteEnv(t *testing.T) *remoteEnv {
	t.Helper()
	for _, tool := range []string{"git", "bash", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	e := &remoteEnv{root: t.TempDir()}
	root := e.root

	e.remote = filepath.Join(root, "remote", "gnolang", "gno.git")
	require.NoError(t, os.MkdirAll(e.remote, 0o755))
	git(t, e.remote, "init", "-q")
	git(t, e.remote, "config", "uploadpack.allowAnySHA1InWant", "true")

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

	e.cfg = &Config{
		DataDir:      filepath.Join(root, "data"),
		Chain:        ChainConfig{GoldenDir: golden, Genesis: genesis},
		GitHub:       GitHubConfig{GitURL: "file://" + filepath.Join(root, "remote") + "/"},
		Repos:        map[string]RepoConfig{"gnolang/gno": {PublicReports: true}},
		Job:          JobConfig{GnoreplayOverlay: overlay},
		DigitalOcean: &DigitalOceanConfig{},
	}
	e.cfg.setDefaults()
	var err error
	e.q, err = openQueue(filepath.Join(t.TempDir(), "jobs.db"))
	require.NoError(t, err)
	t.Cleanup(func() { e.q.Close() })
	e.machines = newFakeMachines(t.TempDir())
	workerWorkDir = filepath.Join(root, "workers")
	// Never schedule this machine's shutdown.
	workerPowerOff = false
	workerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(workerSrv.Close)
	e.cfg.DigitalOcean.URL = workerSrv.URL
	e.restart()
	return e
}

// restart replaces the server with a new process's, on the same queue,
// machines and worker endpoint.
func (e *remoteEnv) restart() *Server {
	e.srv = newServer(e.cfg, e.q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.srv.prov = e.machines
	e.handler.Store(e.srv.workerRoutes())
	return e.srv
}

func TestRemoteReplay(t *testing.T) {
	e := newRemoteEnv(t)
	cfg, q, srv, machines, remote := e.cfg, e.q, e.srv, e.machines, e.remote

	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, Diffs: []Diff{}}
	shaClean := commitReport(t, remote, clean, 0)
	shaCrash := commitReport(t, remote, clean, 1) // the tool exits 1
	require.NoError(t, os.WriteFile(filepath.Join(remote, "fake-sleep"), []byte("1h"), 0o644))
	shaSlow := commitReport(t, remote, clean, 0)

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
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/worker/%d/source", cfg.DigitalOcean.URL, slow.ID), nil)
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

	// A worker older than max_age is deleted even though its job is still
	// running; the job fails with the reason, for good (not requeued).
	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, shaSlow))
	stuck, err := q.Claim()
	require.NoError(t, err)
	require.Equal(t, shaSlow, stuck.SHA)
	finished = make(chan struct{})
	go func() {
		defer close(finished)
		srv.runJob(ctx, stuck)
	}()
	require.Eventually(t, func() bool { _, _, n := machines.names(); return n == 1 }, 30*time.Second, 50*time.Millisecond)
	machines.age(machineName(stuck.ID), cfg.DigitalOcean.MaxAge.Duration+time.Minute)
	require.NoError(t, srv.reap(ctx))
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("job of an over-age worker did not stop")
	}
	_, deleted, running = machines.names()
	assert.Contains(t, deleted, machineName(stuck.ID))
	assert.Zero(t, running)
	got, err = q.Get(stuck.ID)
	require.NoError(t, err)
	assert.Equal(t, stateFailed, got.State)
	assert.Contains(t, got.Error, "exceeded its max age (4h0m0s)")
	pending, err := q.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// A restarted server resumes the jobs its predecessor left on worker
// machines, instead of failing them or starting over.
func TestRemoteResume(t *testing.T) {
	e := newRemoteEnv(t)
	q, machines := e.q, e.machines
	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, Diffs: []Diff{}}
	gate := filepath.Join(e.root, "gate")
	require.NoError(t, os.WriteFile(filepath.Join(e.remote, "fake-wait"), []byte(gate), 0o644))
	shaGated := commitReport(t, e.remote, clean, 0)

	// The first process starts the job, then stops.
	srv := e.srv
	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, shaGated))
	j, err := q.Claim()
	require.NoError(t, err)
	ctx, stop := context.WithCancelCause(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		srv.runJob(ctx, j)
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(workerWorkDir, fmt.Sprint(j.ID), "src", "fake-wait"))
		return err == nil // the worker has the source
	}, 30*time.Second, 50*time.Millisecond)
	stop(errShutdown)
	<-stopped

	// Its job is left running, with its machine, session and job dir.
	got, err := q.Get(j.ID)
	require.NoError(t, err)
	assert.Equal(t, stateRunning, got.State)
	_, deleted, running := machines.names()
	assert.Empty(t, deleted)
	assert.Equal(t, 1, running)
	stored, err := q.Sessions()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, j.ID, stored[0].JobID)
	assert.DirExists(t, e.srv.jobDir(j.ID))

	// Meanwhile: a job interrupted before it had a machine, and a machine
	// no job owns.
	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "chain/mainnet", 0, shaGated))
	early, err := q.Claim()
	require.NoError(t, err)
	machines.orphan("gnoreplay-job-999")

	// The next process resumes the first job, queues the second again and
	// deletes the orphan.
	srv = e.restart()
	ctx = context.Background()
	resume, err := srv.start(ctx)
	require.NoError(t, err)
	require.Len(t, resume, 1)
	assert.Equal(t, j.ID, resume[0].job.ID)
	got, err = q.Get(early.ID)
	require.NoError(t, err)
	assert.Equal(t, stateQueued, got.State)
	_, deleted, running = machines.names()
	assert.Equal(t, []string{"gnoreplay-job-999"}, deleted)
	assert.Equal(t, 1, running)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		srv.resumeJob(ctx, resume[0])
	}()
	require.NoError(t, os.WriteFile(gate, nil, 0o644))
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("resumed job did not finish")
	}
	got, err = q.Get(j.ID)
	require.NoError(t, err)
	require.Equal(t, stateDone, got.State, got.Error)
	assert.Equal(t, outcomePass, got.Outcome)
	_, deleted, running = machines.names()
	assert.Contains(t, deleted, machineName(j.ID))
	assert.Zero(t, running)
	stored, err = q.Sessions()
	require.NoError(t, err)
	assert.Empty(t, stored)
	assert.NoDirExists(t, srv.jobDir(j.ID))
}

// A result saved just before a restart is used, not waited for again.
func TestAwaitWorkerSavedResult(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	cfg.setDefaults()
	srv := newServer(cfg, newTestQueue(t), newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	machines := newFakeMachines(t.TempDir())
	srv.prov = machines
	machines.orphan("gnoreplay-job-1")
	ms, err := machines.List(context.Background())
	require.NoError(t, err)

	dir := t.TempDir()
	sess := &session{dir: dir, created: time.Now(), done: make(chan struct{}, 1)}
	require.NoError(t, os.WriteFile(filepath.Join(dir, workerFailed), []byte("boom"), 0o644))
	err = srv.awaitWorker(context.Background(), sess, ms[0], io.Discard)
	assert.ErrorContains(t, err, "worker failed:\nboom")
	_, deleted, _ := machines.names()
	assert.Equal(t, []string{"gnoreplay-job-1"}, deleted)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.json"), []byte("{}"), 0o644))
	assert.NoError(t, srv.awaitWorker(context.Background(), sess, Machine{ID: "gone"}, io.Discard))
}

func TestWorkerScriptQuoting(t *testing.T) {
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
}

func TestWorkerScriptPowerOff(t *testing.T) {
	render := func(minutes string) string {
		var b strings.Builder
		require.NoError(t, workerTmpl.Execute(&b, map[string]string{
			"URL": "http://10.0.0.1:8081", "JobID": "1", "Token": "t", "WorkDir": "/w",
			"GoVersion": "1.26.1", "GoMemLimit": "12GiB", "PowerOffMinutes": minutes,
		}))
		return b.String()
	}
	// The worker powers itself off at its max age...
	assert.Contains(t, render("240"), `if [ 240 -gt 0 ]; then
	shutdown -P +240`)
	// ...unless disabled (tests).
	assert.Contains(t, render("0"), "if [ 0 -gt 0 ]")
}
