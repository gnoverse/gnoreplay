package main

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Remote replays run on worker machines (see Provisioner), one job at a time:
// the machine is the sandbox. It downloads the job's inputs from the
// coordinator's private worker endpoints with a per-job token, replays, and
// posts back the report. Workers get no other credentials. The provider bills
// started hours: a worker that finished its job is kept for the next one,
// reinstalled from scratch first, so that nothing of a job (PR code runs as
// root) reaches the next; one left idle is deleted in the last minutes of its
// billing hour.

//go:embed worker.sh
var workerScript string

var workerTmpl = template.Must(template.New("worker").Funcs(template.FuncMap{
	"q": shellQuote,
}).Parse(workerScript))

// workerWorkDir is where workers put the job's files.
var workerWorkDir = "/root/gnoreplay"

// workerPowerOff makes workers schedule their own power-off at max_age.
// Tests, which run the worker script on the test machine, turn it off.
var workerPowerOff = true

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// session is a job running on a worker machine. What it needs survives a
// restart: its token hash and creation time are in the sessions table, the
// rest in the job dir, so a restarted server resumes the job (see start).
type session struct {
	tokenHash []byte // SHA-256 of the worker's token
	// dir is the job dir: the source is served from src/, and the worker's
	// result is saved as report.json, or workerFailed.
	dir     string
	created time.Time     // the run timeout counts from here
	done    chan struct{} // signaled when the worker posts its result
}

const workerFailed = "worker-failed.log"

// workerMachine is a worker machine this process knows: running job, or idle
// (job 0), since since.
type workerMachine struct {
	Machine
	job   int64
	since time.Time
}

// idleMargin: an idle worker is deleted this close to the end of its billing
// hour, unless a job takes it first.
const idleMargin = 5 * time.Minute

// hourLeft is what remains of a machine's current billing hour.
func hourLeft(created, now time.Time) time.Duration {
	return time.Hour - now.Sub(created)%time.Hour
}

func newMachineName() string { return "gnoreplay-worker-" + strings.ToLower(rand.Text()[:8]) }

func (s *Server) addSession(id int64, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
}

// endSession forgets a job's session, unless the server is stopping: the
// next start resumes it.
func (s *Server) endSession(ctx context.Context, id int64) {
	if shuttingDown(ctx) {
		return
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	if err := s.queue.DeleteSession(id); err != nil {
		s.logger.Error("delete session", "job", id, "err", err)
	}
}

// replayRemote runs the job on a worker machine, which saves its report as
// report.json in dir.
func (s *Server) replayRemote(ctx context.Context, j *Job, dir string, log io.Writer) error {
	token := rand.Text()
	hash := sha256.Sum256([]byte(token))
	sess := &session{tokenHash: hash[:], dir: dir, created: time.Now(), done: make(chan struct{}, 1)}
	// Saved before the machine exists: a restart then finds the machine and
	// resumes the job, or does not and runs it again.
	if err := s.queue.SaveSession(StoredSession{JobID: j.ID, TokenHash: sess.tokenHash, Created: sess.created}); err != nil {
		return err
	}
	s.addSession(j.ID, sess)
	defer s.endSession(ctx, j.ID)

	var userData strings.Builder
	h := s.cfg.Hetzner
	powerOff := 0
	if workerPowerOff {
		powerOff = int(h.MaxAge.Minutes())
	}
	if err := workerTmpl.Execute(&userData, map[string]string{
		"URL": strings.TrimRight(h.URL, "/"), "JobID": strconv.FormatInt(j.ID, 10), "Token": token,
		"WorkDir":   filepath.Join(workerWorkDir, strconv.FormatInt(j.ID, 10)),
		"GoVersion": h.GoVersion, "GoMemLimit": h.GoMemLimit,
		"PowerOffMinutes": strconv.Itoa(powerOff),
	}); err != nil {
		return err
	}
	m, err := s.assignMachine(ctx, j.ID, userData.String(), sess.created.Add(s.cfg.Job.RunTimeout.Duration), log)
	if err != nil {
		return err
	}
	return s.awaitWorker(ctx, sess, m, log)
}

// createRetry is how often a job retries creating its worker machine.
var createRetry = time.Minute

// assignMachine starts userData on a worker machine for the job: the idle one
// whose billing hour ends first (that hour is paid for already), reinstalled,
// or else a new one. The provider may refuse new machines for a while (out of
// capacity, a project limit, an outage): the job then keeps its place and
// tries again, taking a machine that becomes idle meanwhile, until deadline.
func (s *Server) assignMachine(ctx context.Context, jobID int64, userData string, deadline time.Time, log io.Writer) (Machine, error) {
	for {
		if m, ok, err := s.reuseMachine(ctx, jobID, userData, log); ok || err != nil {
			return m, err
		}
		m, err := s.prov.Create(ctx, newMachineName(), userData, workerLabels(jobID, time.Now()))
		if err == nil {
			s.mu.Lock()
			s.machines[m.ID] = &workerMachine{Machine: m, job: jobID, since: time.Now()}
			s.mu.Unlock()
			fmt.Fprintf(log, "worker machine %s (%s) created\n", m.Name, m.ID)
			return m, nil
		}
		if ctx.Err() != nil {
			return Machine{}, ctx.Err()
		}
		if time.Now().Add(createRetry).After(deadline) {
			return Machine{}, fmt.Errorf("create worker: %w", err)
		}
		s.logger.Error("create worker machine: retrying", "job", jobID, "in", createRetry, "err", err)
		fmt.Fprintf(log, "worker machine not created, retrying in %s: %v\n", createRetry, err)
		select {
		case <-ctx.Done():
			return Machine{}, ctx.Err()
		case <-time.After(createRetry):
		}
	}
}

// reuseMachine gives the job an idle machine, reinstalled, if there is one
// that can be.
func (s *Server) reuseMachine(ctx context.Context, jobID int64, userData string, log io.Writer) (Machine, bool, error) {
	for {
		m, ok := s.takeIdle(jobID)
		if !ok {
			return Machine{}, false, nil
		}
		err := s.prov.Rebuild(ctx, m.ID, userData)
		if err == nil {
			// Only once it runs the job: a restart finds the job's machine
			// by this label.
			err = s.prov.SetLabels(ctx, m.ID, workerLabels(jobID, time.Now()))
		}
		if err == nil {
			fmt.Fprintf(log, "worker machine %s (%s) reinstalled\n", m.Name, m.ID)
			return m, true, nil
		}
		if ctx.Err() != nil {
			s.releaseMachine(ctx, m, log)
			return Machine{}, false, ctx.Err()
		}
		s.logger.Error("reinstall worker machine", "machine", m.Name, "err", err)
		fmt.Fprintf(log, "worker machine %s (%s) not reinstalled: %v\n", m.Name, m.ID, err)
		s.deleteMachine(m, log)
	}
}

// takeIdle gives the job the idle machine whose billing hour ends first.
func (s *Server) takeIdle(jobID int64) (Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var best *workerMachine
	for _, w := range s.machines {
		if w.job == 0 && (best == nil || hourLeft(w.Created, now) < hourLeft(best.Created, now)) {
			best = w
		}
	}
	if best == nil {
		return Machine{}, false
	}
	best.job, best.since = jobID, now
	return best.Machine, true
}

// releaseMachine makes a job's machine idle: the next job reinstalls it, or
// it is deleted at the end of its billing hour. A stopping server leaves it
// to the next start.
func (s *Server) releaseMachine(ctx context.Context, m Machine, log io.Writer) {
	if shuttingDown(ctx) {
		return
	}
	s.mu.Lock()
	_, known := s.machines[m.ID]
	s.mu.Unlock()
	if !known {
		return // deleted meanwhile, over its max age
	}
	// Labelled before another job can take it, not to overwrite that job's
	// labels.
	now := time.Now()
	labelCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := s.prov.SetLabels(labelCtx, m.ID, workerLabels(0, now))
	cancel()
	if err != nil {
		// deploy/watchdog.sh then takes it for busy, until its max age.
		s.logger.Error("label idle worker machine", "machine", m.Name, "err", err)
	}
	s.mu.Lock()
	if w := s.machines[m.ID]; w != nil {
		w.job, w.since = 0, now
	}
	s.mu.Unlock()
	fmt.Fprintf(log, "worker machine %s released\n", m.Name)
}

// awaitWorker waits for the worker's result, then releases its machine. A
// stopping server leaves the machine running for the next start to resume.
func (s *Server) awaitWorker(ctx context.Context, sess *session, m Machine, log io.Writer) error {
	defer s.releaseMachine(ctx, m, log)
	timeout := time.NewTimer(time.Until(sess.created.Add(s.cfg.Job.RunTimeout.Duration)))
	defer timeout.Stop()
	for {
		// Checked first: the result may have come before a restart.
		if done, err := sess.result(log); done {
			return err
		}
		select {
		case <-sess.done:
		case <-ctx.Done():
			if done, err := sess.result(log); done {
				return err
			}
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("worker did not report within %s", s.cfg.Job.RunTimeout.Duration)
		}
	}
}

// result reports whether the worker posted its result, and the error if it
// failed.
func (sess *session) result(log io.Writer) (bool, error) {
	if _, err := os.Stat(filepath.Join(sess.dir, "report.json")); err == nil {
		return true, nil
	}
	bz, err := os.ReadFile(filepath.Join(sess.dir, workerFailed))
	if err != nil {
		return false, nil
	}
	fmt.Fprintf(log, "worker log:\n%s\n", bz)
	return true, fmt.Errorf("worker failed:\n%s", tail(string(bz), 4<<10))
}

// resumable is a job a previous server process left on a worker machine.
type resumable struct {
	job     *Job
	sess    *session
	machine Machine
}

// start readies a new server process: jobs left on worker machines that still
// run them are resumed (the returned ones), other interrupted jobs are queued
// again, and the other machines are idle.
func (s *Server) start(ctx context.Context) ([]resumable, error) {
	var resume []resumable
	if s.prov != nil {
		stored, err := s.queue.Sessions()
		if err != nil {
			return nil, err
		}
		machines, err := s.prov.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list worker machines: %w", err)
		}
		byJob := map[int64]Machine{}
		for _, m := range machines {
			if job, err := strconv.ParseInt(m.Labels[labelJob], 10, 64); err == nil {
				byJob[job] = m
			}
		}
		for _, st := range stored {
			j, err := s.queue.Get(st.JobID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			running := err == nil && j.State == stateRunning
			m, ok := byJob[st.JobID]
			if running && ok {
				sess := &session{tokenHash: st.TokenHash, dir: s.jobDir(j.ID), created: st.Created, done: make(chan struct{}, 1)}
				s.addSession(j.ID, sess)
				s.mu.Lock()
				s.machines[m.ID] = &workerMachine{Machine: m, job: j.ID, since: st.Created}
				s.mu.Unlock()
				resume = append(resume, resumable{job: j, sess: sess, machine: m})
				continue
			}
			// The job ended, or no machine runs it (never given one, or
			// deleted at its max age): the job runs again (Requeue below),
			// unless it ran out of time.
			if err := s.queue.DeleteSession(st.JobID); err != nil {
				return nil, err
			}
			if running && time.Since(st.Created) > s.cfg.Job.RunTimeout.Duration {
				if _, err := s.queue.Finish(j.ID, Job{State: stateFailed, Error: fmt.Sprintf(
					"no worker machine runs the job anymore, and it ran out of time (%s)", s.cfg.Job.RunTimeout.Duration)}); err != nil {
					return nil, err
				}
			}
		}
	}
	if n, err := s.queue.Requeue(); err != nil {
		return nil, err
	} else if n > 0 {
		s.logger.Info("requeued interrupted jobs", "count", n)
	}
	if s.prov != nil {
		// The other machines become idle ones.
		if err := s.reap(ctx); err != nil {
			return nil, fmt.Errorf("reap worker machines: %w", err)
		}
	}
	for _, r := range resume {
		s.logger.Info("resuming job on its worker machine", "job", r.job.ID, "machine", r.machine.Name)
	}
	return resume, nil
}

// resumeJob waits for a job a previous server process left on its machine.
func (s *Server) resumeJob(ctx context.Context, r resumable) {
	j := r.job
	s.track(ctx, j, func(ctx context.Context, logger *slog.Logger) (*Report, string, error) {
		defer s.removeJobDir(ctx, r.sess.dir)
		defer s.endSession(ctx, j.ID)
		logFile, err := os.OpenFile(filepath.Join(r.sess.dir, "job.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			s.releaseMachine(ctx, r.machine, io.Discard)
			return nil, "", err
		}
		defer logFile.Close()
		// Cancelled before this process tracked it: nothing to wait for.
		if cur, err := s.queue.Get(j.ID); err == nil && cur.State != stateRunning {
			s.releaseMachine(ctx, r.machine, logFile)
			return nil, "", errSuperseded
		}
		logger.Info("replay resumed", "machine", r.machine.Name)
		fmt.Fprintf(logFile, "==> resumed by a new server process\n")
		if err := s.awaitWorker(ctx, r.sess, r.machine, logFile); err != nil {
			return nil, "", err
		}
		return s.saveReport(j, filepath.Join(r.sess.dir, "report.json"))
	})
}

func (s *Server) deleteMachine(m Machine, log io.Writer) {
	s.mu.Lock()
	delete(s.machines, m.ID)
	s.mu.Unlock()
	var err error
	for attempt := range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err = s.prov.Delete(ctx, m.ID)
		cancel()
		if err == nil {
			fmt.Fprintf(log, "worker machine %s deleted\n", m.Name)
			return
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Second)
	}
	// The reaper finds it again, as an idle machine.
	s.logger.Error("delete worker machine", "machine", m.Name, "id", m.ID, "err", err)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// reap keeps the worker machines in check. It runs at startup, before any
// job, and then every minute:
//   - a machine that has run its job for longer than max_age is deleted, and
//     its job cancelled;
//   - an idle one is deleted in the last minutes of its billing hour;
//   - one this process doesn't know (left by a previous one, or by a failed
//     create) becomes an idle one.
//
// (deploy/watchdog.sh deletes machines busy or idle for too long too,
// without the server.)
func (s *Server) reap(ctx context.Context) error {
	machines, err := s.prov.List(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	maxAge := s.cfg.Hetzner.MaxAge.Duration
	var overAge []workerMachine
	var expired []Machine
	listed := map[string]bool{}
	s.mu.Lock()
	for _, m := range machines {
		listed[m.ID] = true
		w := s.machines[m.ID]
		if w == nil {
			if job, err := strconv.ParseInt(m.Labels[labelJob], 10, 64); err == nil && s.sessions[job] != nil {
				continue // created for a job, which records it once created
			}
			w = &workerMachine{Machine: m, since: now}
			s.machines[m.ID] = w
			s.logger.Info("idle worker machine found", "machine", m.Name, "created", m.Created)
		}
		switch {
		case w.job != 0 && now.Sub(w.since) > maxAge:
			overAge = append(overAge, *w)
			delete(s.machines, m.ID)
		case w.job == 0 && hourLeft(m.Created, now) <= idleMargin:
			expired = append(expired, m)
			delete(s.machines, m.ID)
		}
	}
	for id, w := range s.machines {
		if !listed[id] && w.job == 0 {
			delete(s.machines, id) // deleted by someone else
		}
	}
	s.mu.Unlock()

	for _, w := range overAge {
		s.logger.Error("worker machine ran its job past its max age: cancelling it",
			"machine", w.Name, "job", w.job, "since", w.since, "max_age", maxAge)
		s.cancelCause(w.job, fmt.Errorf("worker machine %s ran the job for longer than its max age (%s), and was deleted",
			w.Name, maxAge))
		if err := s.prov.Delete(ctx, w.ID); err != nil {
			s.logger.Error("delete worker machine", "machine", w.Name, "err", err)
		}
	}
	for _, m := range expired {
		if err := s.prov.Delete(ctx, m.ID); err != nil {
			s.logger.Error("delete idle worker machine", "machine", m.Name, "err", err)
			continue
		}
		s.logger.Info("deleted idle worker machine at the end of its billing hour", "machine", m.Name, "created", m.Created)
	}
	return nil
}

func (s *Server) reapLoop(ctx context.Context, every time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		if err := s.reap(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("reap", "err", err)
		}
	}
}

// workerRoutes serves workers, on the private network only.
func (s *Server) workerRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /worker/{id}/source", s.withSession(func(w http.ResponseWriter, _ *http.Request, sess *session, _ int64) {
		writeTar(w, map[string]string{".": filepath.Join(sess.dir, "src")})
	}))
	mux.HandleFunc("GET /worker/{id}/golden", s.withSession(func(w http.ResponseWriter, _ *http.Request, _ *session, _ int64) {
		// Resolved once: the golden dir may be swapped to a new snapshot
		// while this is streamed.
		golden, err := filepath.EvalSymlinks(s.cfg.Chain.GoldenDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeTar(w, map[string]string{
			"db/blockstore.db": filepath.Join(golden, "db", "blockstore.db"),
			"db/state.db":      filepath.Join(golden, "db", "state.db"),
		})
	}))
	mux.HandleFunc("GET /worker/{id}/genesis", s.withSession(func(w http.ResponseWriter, r *http.Request, _ *session, _ int64) {
		http.ServeFile(w, r, s.cfg.Chain.Genesis)
	}))
	// Results are saved in the job dir before they are acknowledged, so a
	// restart in between loses nothing.
	mux.HandleFunc("POST /worker/{id}/report", s.withSession(func(w http.ResponseWriter, r *http.Request, sess *session, _ int64) {
		if err := saveUpload(filepath.Join(sess.dir, "report.json"), http.MaxBytesReader(w, r.Body, 1<<30)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sess.signal()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /worker/{id}/fail", s.withSession(func(w http.ResponseWriter, r *http.Request, sess *session, _ int64) {
		if err := saveUpload(filepath.Join(sess.dir, workerFailed), http.MaxBytesReader(w, r.Body, 4<<20)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sess.signal()
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// saveUpload writes body to path atomically: path exists only once complete.
func saveUpload(path string, body io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.upload")
	if err != nil {
		return err
	}
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (sess *session) signal() {
	select {
	case sess.done <- struct{}{}:
	default: // already signaled
	}
}

// withSession authenticates a worker request: the job must be waiting for
// its worker, and the token must be the job's.
func (s *Server) withSession(h func(http.ResponseWriter, *http.Request, *session, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		sess := s.sessions[id]
		s.mu.Unlock()
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		hash := sha256.Sum256([]byte(token))
		if sess == nil || subtle.ConstantTimeCompare(hash[:], sess.tokenHash) != 1 {
			http.NotFound(w, r)
			return
		}
		h(w, r, sess, id)
	}
}

// writeTar streams the given directories (archive path -> directory) as an
// uncompressed tar: workers download over the private network, where
// bandwidth is cheaper than compression.
func writeTar(w http.ResponseWriter, dirs map[string]string) {
	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	for prefix, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return nil // no symlinks or devices
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(filepath.Join(prefix, rel))
			if d.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		if err != nil {
			// Headers are sent: break the stream so the worker's tar fails.
			panic(http.ErrAbortHandler)
		}
	}
	if err := tw.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
}
