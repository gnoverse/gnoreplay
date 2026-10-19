package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
// server's cloud-init would, rebuilds it by killing that script and running
// the new user data, and deletes it by killing the script.
type fakeMachines struct {
	dir string

	mu          sync.Mutex
	nextID      int
	boots       int
	running     map[string]*fakeMachine // by ID
	created     []string                // names
	rebuilt     []string                // names, at each rebuild
	deleted     []string                // names
	failRebuild bool
	failCreate  int // the next creates that fail
}

type fakeMachine struct {
	name    string
	created time.Time
	labels  map[string]string
	cancel  context.CancelFunc
	done    chan struct{}
}

func newFakeMachines(dir string) *fakeMachines {
	return &fakeMachines{dir: dir, running: map[string]*fakeMachine{}}
}

// boot runs userData on m, with f.mu held.
func (f *fakeMachines) boot(m *fakeMachine, userData string) error {
	f.boots++
	script := filepath.Join(f.dir, fmt.Sprintf("%s-%d.sh", m.name, f.boots))
	if err := os.WriteFile(script, []byte(userData), 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel, m.done = cancel, done
	go func() {
		defer close(done)
		exec.CommandContext(ctx, "bash", script).Run()
	}()
	return nil
}

func (m *fakeMachine) machine(id string) Machine {
	return Machine{ID: id, Name: m.name, Created: m.created, Labels: maps.Clone(m.labels)}
}

func (f *fakeMachines) Create(_ context.Context, name, userData string, labels map[string]string) (Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate > 0 {
		f.failCreate--
		return Machine{}, errors.New("shared core limit exceeded")
	}
	f.nextID++
	id := strconv.Itoa(f.nextID)
	m := &fakeMachine{name: name, created: time.Now(), labels: maps.Clone(labels)}
	if err := f.boot(m, userData); err != nil {
		return Machine{}, err
	}
	f.running[id] = m
	f.created = append(f.created, name)
	return m.machine(id), nil
}

func (f *fakeMachines) Rebuild(_ context.Context, id, userData string) error {
	f.mu.Lock()
	m, ok := f.running[id]
	if !ok || f.failRebuild {
		f.mu.Unlock()
		return fmt.Errorf("rebuild %s: %w", id, errors.ErrUnsupported)
	}
	cancel, done := m.cancel, m.done
	f.mu.Unlock()
	cancel()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuilt = append(f.rebuilt, m.name)
	return f.boot(m, userData)
}

func (f *fakeMachines) SetLabels(_ context.Context, id string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.running[id]
	if !ok {
		return fmt.Errorf("machine %s not found", id)
	}
	m.labels = maps.Clone(labels)
	return nil
}

func (f *fakeMachines) Find(_ context.Context, name string) (*Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.running {
		if m.name == name {
			machine := m.machine(id)
			return &machine, nil
		}
	}
	return nil, nil
}

// orphan registers a machine this process doesn't know, as if left by a
// previous one: running job, or idle (job 0).
func (f *fakeMachines) orphan(name string, job int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	done := make(chan struct{})
	close(done)
	f.running[strconv.Itoa(f.nextID)] = &fakeMachine{
		name: name, created: time.Now(), labels: workerLabels(job, time.Now()), cancel: func() {}, done: done,
	}
}

func (f *fakeMachines) labels(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.running {
		if m.name == name {
			return maps.Clone(m.labels)
		}
	}
	return nil
}

func (f *fakeMachines) rebuilds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rebuilt...)
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
		out = append(out, m.machine(id))
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
		DataDir: filepath.Join(root, "data"),
		Chain:   ChainConfig{GoldenDir: golden, Genesis: genesis},
		GitHub:  GitHubConfig{GitURL: "file://" + filepath.Join(root, "remote") + "/"},
		Repos:   map[string]RepoConfig{"gnolang/gno": {PublicReports: true}},
		Job:     JobConfig{GnoreplayOverlay: overlay},
		Hetzner: &HetznerConfig{},
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
	workerProgressInterval = 50 * time.Millisecond
	workerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(workerSrv.Close)
	e.cfg.Hetzner.URL = workerSrv.URL
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

// backdate makes the machine running job look like it got it d earlier.
func backdate(srv *Server, job int64, d time.Duration) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, w := range srv.machines {
		if w.job == job {
			w.since = w.since.Add(-d)
		}
	}
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
	// start runs a job in the background, until it is on its machine.
	start := func(sha string) (*Job, chan struct{}) {
		t.Helper()
		require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, sha))
		j, err := q.Claim()
		require.NoError(t, err)
		require.Equal(t, sha, j.SHA)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			srv.runJob(ctx, j)
		}()
		require.Eventually(t, func() bool {
			ms, _ := machines.List(ctx)
			for _, m := range ms {
				if m.Labels[labelJob] == strconv.FormatInt(j.ID, 10) {
					return true
				}
			}
			return false
		}, 30*time.Second, 50*time.Millisecond)
		return j, finished
	}
	wait := func(finished chan struct{}) {
		t.Helper()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Fatal("job did not stop")
		}
	}

	// A replay on a new worker machine, which is kept, idle, once it
	// reported.
	ok := run(shaClean)
	require.Equal(t, stateDone, ok.State, ok.Error)
	assert.Equal(t, outcomePass, ok.Outcome)
	report, err := readReport(ok.ReportPath)
	require.NoError(t, err)
	assert.EqualValues(t, 50, report.Blocks)
	created, deleted, running := machines.names()
	require.Len(t, created, 1)
	worker := created[0]
	assert.Empty(t, deleted)
	assert.Equal(t, 1, running)
	assert.Equal(t, "worker", machines.labels(worker)[labelRole])
	assert.NotContains(t, machines.labels(worker), labelJob, "idle")
	// The session is gone: its token no longer works.
	assert.Empty(t, srv.sessions)

	// The next job runs on it, reinstalled. The worker fails: the job fails
	// with the worker's log, and the machine is idle again.
	failed := run(shaCrash)
	assert.Equal(t, stateFailed, failed.State)
	assert.Contains(t, failed.Error, "gnoreplay exited with 1")
	assert.Contains(t, failed.Error, "==> replay")
	created, deleted, running = machines.names()
	assert.Equal(t, []string{worker}, created)
	assert.Equal(t, []string{worker}, machines.rebuilds())
	assert.Empty(t, deleted)
	assert.Equal(t, 1, running)
	assert.NotContains(t, machines.labels(worker), labelJob)

	// A slow replay, on it again. Its worker reports its progress, which
	// pages and the queue show.
	slow, finished := start(shaSlow)
	assert.Equal(t, strconv.FormatInt(slow.ID, 10), machines.labels(worker)[labelJob])
	require.Eventually(t, func() bool {
		j, err := q.Get(slow.ID)
		return err == nil && j.Progress != nil
	}, 30*time.Second, 50*time.Millisecond)
	got, err := q.Get(slow.ID)
	require.NoError(t, err)
	assert.Equal(t, "replaying: block 25 of 50 (50%), about 5s left", result(got))
	assert.WithinDuration(t, time.Now(), got.Progress.At, time.Minute)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/queue", nil))
	assert.Contains(t, rec.Body.String(), `"progress":{"height":25,"tip":50,"blocks_per_second":5,`)

	// The reaper spares a busy machine at the end of its billing hour, and
	// takes one it doesn't know for an idle one...
	machines.orphan("gnoreplay-worker-orphan", 0)
	machines.age(worker, 56*time.Minute)
	require.NoError(t, srv.reap(ctx))
	_, deleted, running = machines.names()
	assert.Empty(t, deleted)
	assert.Equal(t, 2, running)
	// ...which it deletes in the last minutes of its billing hour.
	machines.age("gnoreplay-worker-orphan", time.Hour+56*time.Minute)
	require.NoError(t, srv.reap(ctx))
	_, deleted, running = machines.names()
	assert.Equal(t, []string{"gnoreplay-worker-orphan"}, deleted)
	assert.Equal(t, 1, running)

	// Worker endpoints need the job's token.
	for _, token := range []string{"", "wrong"} {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/worker/%d/source", cfg.Hetzner.URL, slow.ID), nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		res.Body.Close()
		assert.Equal(t, http.StatusNotFound, res.StatusCode, "token %q", token)
	}

	// Superseded by a new push: the job stops, and its machine is idle.
	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, shaClean))
	wait(finished)
	got, err = q.Get(slow.ID)
	require.NoError(t, err)
	assert.Equal(t, stateSuperseded, got.State)
	_, deleted, running = machines.names()
	assert.NotContains(t, deleted, worker)
	assert.Equal(t, 1, running)
	assert.NotContains(t, machines.labels(worker), labelJob)
	queued, err := q.Claim()
	require.NoError(t, err)
	_, err = q.Finish(queued.ID, Job{State: stateSuperseded})
	require.NoError(t, err)

	// Of two idle machines, a job takes the one whose billing hour ends
	// first: it is paid for until then.
	machines.age(worker, 30*time.Minute)
	machines.orphan("gnoreplay-worker-soon", 0)
	machines.age("gnoreplay-worker-soon", 50*time.Minute)
	require.NoError(t, srv.reap(ctx))
	ok = run(shaClean)
	require.Equal(t, stateDone, ok.State, ok.Error)
	assert.Equal(t, []string{worker, worker, "gnoreplay-worker-soon"}, machines.rebuilds())

	// Machines that can't be reinstalled are deleted, and the job gets a new
	// one.
	machines.mu.Lock()
	machines.failRebuild = true
	machines.mu.Unlock()
	ok = run(shaClean)
	require.Equal(t, stateDone, ok.State, ok.Error)
	created, deleted, running = machines.names()
	assert.ElementsMatch(t, []string{"gnoreplay-worker-orphan", worker, "gnoreplay-worker-soon"}, deleted)
	require.Len(t, created, 2)
	assert.Equal(t, 1, running)
	worker = created[1]
	machines.mu.Lock()
	machines.failRebuild = false
	machines.mu.Unlock()

	// While the provider refuses new machines, a job keeps its place and
	// retries, until its run timeout.
	createRetry = 10 * time.Millisecond
	t.Cleanup(func() { createRetry = time.Minute })
	machines.mu.Lock()
	machines.failCreate = 3
	machines.mu.Unlock()
	machines.age(worker, time.Hour+56*time.Minute)
	require.NoError(t, srv.reap(ctx)) // no idle machine left
	ok = run(shaClean)
	require.Equal(t, stateDone, ok.State, ok.Error)
	created, _, running = machines.names()
	require.Len(t, created, 3)
	assert.Equal(t, 1, running)
	worker = created[2]
	machines.mu.Lock()
	machines.failCreate = 1 << 30
	machines.mu.Unlock()
	machines.age(worker, time.Hour+56*time.Minute)
	require.NoError(t, srv.reap(ctx))
	cfg.Job.RunTimeout.Duration = 50 * time.Millisecond
	failed = run(shaClean)
	cfg.Job.RunTimeout.Duration = 3*time.Hour + 30*time.Minute
	assert.Equal(t, stateFailed, failed.State)
	assert.Contains(t, failed.Error, "create worker: shared core limit exceeded")
	machines.mu.Lock()
	machines.failCreate = 0
	machines.mu.Unlock()

	// A job that has run on its worker for longer than max_age is cancelled,
	// and the worker deleted; the job fails with the reason, for good (not
	// requeued).
	stuck, finished := start(shaSlow)
	created, _, _ = machines.names()
	worker = created[len(created)-1]
	assert.Equal(t, strconv.FormatInt(stuck.ID, 10), machines.labels(worker)[labelJob])
	backdate(srv, stuck.ID, cfg.Hetzner.MaxAge.Duration+time.Minute)
	require.NoError(t, srv.reap(ctx))
	wait(finished)
	_, deleted, running = machines.names()
	assert.Contains(t, deleted, worker)
	assert.Zero(t, running)
	got, err = q.Get(stuck.ID)
	require.NoError(t, err)
	assert.Equal(t, stateFailed, got.State)
	assert.Contains(t, got.Error, "longer than its max age (4h0m0s)")
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
	created, deleted, running := machines.names()
	require.Len(t, created, 1)
	worker := created[0]
	assert.Empty(t, deleted)
	assert.Equal(t, 1, running)
	assert.Equal(t, strconv.FormatInt(j.ID, 10), machines.labels(worker)[labelJob])
	stored, err := q.Sessions()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, j.ID, stored[0].JobID)
	assert.DirExists(t, e.srv.jobDir(j.ID))

	// Meanwhile: a job interrupted before it had a machine, and a machine
	// labelled for a job that ended.
	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "chain/mainnet", 0, shaGated))
	early, err := q.Claim()
	require.NoError(t, err)
	machines.orphan("gnoreplay-worker-orphan", 999)

	// The next process resumes the first job, queues the second again and
	// takes the other machine for an idle one.
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
	assert.Empty(t, deleted)
	assert.Equal(t, 2, running)
	m, ok := srv.takeIdle(early.ID)
	require.True(t, ok)
	assert.Equal(t, "gnoreplay-worker-orphan", m.Name)
	_, ok = srv.takeIdle(early.ID)
	assert.False(t, ok, "the resumed job's machine is busy")

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
	_, deleted, _ = machines.names()
	assert.Empty(t, deleted)
	assert.NotContains(t, machines.labels(worker), labelJob, "idle")
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
	machines.orphan("gnoreplay-worker-1", 1)
	ms, err := machines.List(context.Background())
	require.NoError(t, err)
	srv.machines[ms[0].ID] = &workerMachine{Machine: ms[0], job: 1, since: time.Now()}

	dir := t.TempDir()
	sess := &session{dir: dir, created: time.Now(), done: make(chan struct{}, 1)}
	require.NoError(t, os.WriteFile(filepath.Join(dir, workerFailed), []byte("boom"), 0o644))
	err = srv.awaitWorker(context.Background(), sess, ms[0], io.Discard)
	assert.ErrorContains(t, err, "worker failed:\nboom")
	assert.NotContains(t, machines.labels("gnoreplay-worker-1"), labelJob, "released")
	assert.Zero(t, srv.machines[ms[0].ID].job)

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
			"GoVersion": "1.26.1", "GoMemLimit": "12GiB", "PowerOffMinutes": minutes, "ProgressSeconds": "60",
			"ReplayArgs": "--data-dir 'data'",
		}))
		return b.String()
	}
	// The worker powers itself off at its max age...
	assert.Contains(t, render("240"), `if [ 240 -gt 0 ]; then
	shutdown -P +240`)
	// ...unless disabled (tests).
	assert.Contains(t, render("0"), "if [ 0 -gt 0 ]")
	// The replay command line comes from the server (ReplayArgs).
	assert.Contains(t, render("0"), "./gnoreplay --data-dir 'data'")
}

// A restart without a provisioner, e.g. after removing [hetzner] from the
// config, must not leave the jobs a previous process ran on worker machines
// running forever: their machines are beyond reach, so the sessions go and
// the jobs run again.
func TestStartWithoutProvisionerRequeuesSessions(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	cfg.setDefaults()
	q := newTestQueue(t)
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NoError(t, srv.enqueueFor("gnolang/gno", eventPush, "master", 0, "abc"))
	j, err := q.Claim()
	require.NoError(t, err)
	require.NoError(t, q.SaveSession(StoredSession{JobID: j.ID, TokenHash: []byte("hash"), Created: time.Now()}))

	resume, err := srv.start(context.Background())
	require.NoError(t, err)
	assert.Empty(t, resume)

	got, err := q.Get(j.ID)
	require.NoError(t, err)
	assert.Equal(t, stateQueued, got.State, "the job runs again, locally")
	sessions, err := q.Sessions()
	require.NoError(t, err)
	assert.Empty(t, sessions)
	next, err := q.Claim()
	require.NoError(t, err)
	require.NotNil(t, next)
}

// A machine that disappears while its job runs must be noticed: the job runs
// again, rather than waiting out its run timeout and failing.
func TestRemoteVanishedMachineRequeues(t *testing.T) {
	e := newRemoteEnv(t)
	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, Diffs: []Diff{}}
	gate := filepath.Join(e.root, "gate")
	require.NoError(t, os.WriteFile(filepath.Join(e.remote, "fake-wait"), []byte(gate), 0o644))
	sha := commitReport(t, e.remote, clean, 0)

	require.NoError(t, e.srv.enqueueFor("gnolang/gno", eventPush, "master", 0, sha))
	j, err := e.q.Claim()
	require.NoError(t, err)
	finished := make(chan struct{})
	go func() { e.srv.runJob(context.Background(), j); close(finished) }()

	var id string
	require.Eventually(t, func() bool {
		ms, _ := e.machines.List(context.Background())
		for _, m := range ms {
			if m.Labels[labelJob] == strconv.FormatInt(j.ID, 10) {
				id = m.ID
				return true
			}
		}
		return false
	}, 30*time.Second, 10*time.Millisecond)
	require.NoError(t, e.machines.Delete(context.Background(), id))
	require.NoError(t, e.srv.reap(context.Background()))

	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the job did not stop")
	}
	got, err := e.q.Get(j.ID)
	require.NoError(t, err)
	assert.Equal(t, stateQueued, got.State, "the job runs again")
	pending, err := e.q.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, j.ID, pending[0].ID)
}

type bootThenFailCreate struct {
	*fakeMachines
	lost bool
}

// Create boots the machine, then fails: a create whose response was lost.
func (f *bootThenFailCreate) Create(ctx context.Context, name, userData string, labels map[string]string) (Machine, error) {
	m, err := f.fakeMachines.Create(ctx, name, userData, labels)
	if err != nil || f.lost {
		return m, err
	}
	f.lost = true
	return Machine{}, errors.New("create response lost")
}

// A create that booted its machine but failed to say so is found by name and
// taken: the retry must not leave two machines on one job.
func TestRemoteLostCreateResponse(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	cfg.setDefaults()
	srv := newServer(cfg, newTestQueue(t), newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	machines := newFakeMachines(t.TempDir())
	srv.prov = &bootThenFailCreate{fakeMachines: machines}

	old := createRetry
	createRetry = 5 * time.Millisecond
	defer func() { createRetry = old }()

	m, err := srv.assignMachine(context.Background(), 42, "user data", time.Now().Add(5*time.Second), io.Discard)
	require.NoError(t, err)
	created, _, running := machines.names()
	assert.Len(t, created, 1, "the booted machine is taken, not left behind")
	assert.Equal(t, 1, running)
	assert.Equal(t, "42", machines.labels(m.Name)[labelJob])
}

// A machine adopted as idle after a long outage is relabelled then: its old
// idle-since label would let the watchdog delete it mid-rebuild.
func TestRemoteAdoptedIdleRelabelled(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir(), Hetzner: &HetznerConfig{}}
	cfg.setDefaults()
	srv := newServer(cfg, newTestQueue(t), newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	machines := newFakeMachines(t.TempDir())
	srv.prov = machines

	machines.orphan("gnoreplay-worker-stale", 0)
	machines.mu.Lock()
	for _, m := range machines.running {
		m.labels = workerLabels(0, time.Now().Add(-2*time.Hour))
	}
	machines.mu.Unlock()

	_, err := srv.start(context.Background())
	require.NoError(t, err)
	labels := machines.labels("gnoreplay-worker-stale")
	since, err := strconv.ParseInt(labels[labelSince], 10, 64)
	require.NoError(t, err)
	assert.Greater(t, since, time.Now().Add(-time.Minute).Unix(), "relabelled when adopted")
	assert.NotContains(t, labels, labelJob)
}

// A worker whose log is larger than the fail route's cap used to hang the job
// until its run timeout: the worker posts a bounded tail now.
func TestRemoteLoudWorkerLogFails(t *testing.T) {
	e := newRemoteEnv(t)
	clean := &Report{ChainID: "gnoland-1", FromHeight: 1, ToHeight: 50, Blocks: 50, Txs: 10, Diffs: []Diff{}}
	require.NoError(t, os.WriteFile(filepath.Join(e.remote, "fake-loud"), nil, 0o644))
	sha := commitReport(t, e.remote, clean, 1)

	require.NoError(t, e.srv.enqueueFor("gnolang/gno", eventPush, "master", 0, sha))
	j, err := e.q.Claim()
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { e.srv.runJob(context.Background(), j); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the job did not fail quickly: the worker's log was rejected, and it waits out its run timeout")
	}
	got, err := e.q.Get(j.ID)
	require.NoError(t, err)
	require.Equal(t, stateFailed, got.State)
	assert.Contains(t, got.Error, "loud")
	assert.Contains(t, got.Error, "gnoreplay exited with 1")
}

type permanentFailCreate struct {
	*fakeMachines
	attempts int
}

func (f *permanentFailCreate) Create(context.Context, string, string, map[string]string) (Machine, error) {
	f.attempts++
	return Machine{}, permanentCreateError{errors.New("unauthorized: invalid token")}
}

// A create that retrying cannot fix fails the job at once, instead of holding
// its slot for the whole run timeout.
func TestRemotePermanentCreateFailsFast(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	cfg.setDefaults()
	srv := newServer(cfg, newTestQueue(t), newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	fail := &permanentFailCreate{fakeMachines: newFakeMachines(t.TempDir())}
	srv.prov = fail

	old := createRetry
	createRetry = time.Second
	defer func() { createRetry = old }()

	start := time.Now()
	_, err := srv.assignMachine(context.Background(), 1, "ud", time.Now().Add(10*time.Second), io.Discard)
	assert.ErrorContains(t, err, "unauthorized")
	assert.Equal(t, 1, fail.attempts, "not retried")
	assert.Less(t, time.Since(start), time.Second, "failed at once")
}
