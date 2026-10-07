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

// Remote replays run on a disposable machine per job (see Provisioner): the
// machine is the sandbox. It downloads the job's inputs from the coordinator's
// private worker endpoints with a per-job token, replays, and posts back the
// report; the coordinator then deletes it. Workers get no other credentials.

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

func machineName(jobID int64) string { return fmt.Sprintf("gnoreplay-job-%d", jobID) }

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

// replayRemote runs the job on a new machine, which saves its report as
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
	do := s.cfg.DigitalOcean
	powerOff := 0
	if workerPowerOff {
		powerOff = int(do.MaxAge.Minutes())
	}
	if err := workerTmpl.Execute(&userData, map[string]string{
		"URL": strings.TrimRight(do.URL, "/"), "JobID": strconv.FormatInt(j.ID, 10), "Token": token,
		"WorkDir":   filepath.Join(workerWorkDir, strconv.FormatInt(j.ID, 10)),
		"GoVersion": do.GoVersion, "GoMemLimit": do.GoMemLimit,
		"PowerOffMinutes": strconv.Itoa(powerOff),
	}); err != nil {
		return err
	}
	m, err := s.prov.Create(ctx, machineName(j.ID), userData.String())
	if err != nil {
		return fmt.Errorf("create worker: %w", err)
	}
	fmt.Fprintf(log, "worker machine %s (%s) created\n", m.Name, m.ID)
	return s.awaitWorker(ctx, sess, m, log)
}

// awaitWorker waits for the worker's result, then deletes its machine. A
// stopping server leaves the machine running for the next start to resume.
func (s *Server) awaitWorker(ctx context.Context, sess *session, m Machine, log io.Writer) error {
	// Machines cost money until deleted, whatever happens to the job.
	defer func() {
		if !shuttingDown(ctx) {
			s.deleteMachine(m, log)
		}
	}()
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
// exist are resumed (the returned ones), other interrupted jobs are queued
// again, and machines no job owns are deleted.
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
		byName := map[string]Machine{}
		for _, m := range machines {
			byName[m.Name] = m
		}
		for _, st := range stored {
			j, err := s.queue.Get(st.JobID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			running := err == nil && j.State == stateRunning
			m, ok := byName[machineName(st.JobID)]
			if running && ok {
				sess := &session{tokenHash: st.TokenHash, dir: s.jobDir(j.ID), created: st.Created, done: make(chan struct{}, 1)}
				s.addSession(j.ID, sess)
				resume = append(resume, resumable{job: j, sess: sess, machine: m})
				continue
			}
			// The job ended, or its machine is gone (never created, or
			// deleted at its max age): the job runs again (Requeue below),
			// unless it ran out of time.
			if err := s.queue.DeleteSession(st.JobID); err != nil {
				return nil, err
			}
			if running && time.Since(st.Created) > s.cfg.Job.RunTimeout.Duration {
				if _, err := s.queue.Finish(j.ID, Job{State: stateFailed, Error: fmt.Sprintf(
					"worker machine %s is gone, and the job ran out of time (%s)", machineName(j.ID), s.cfg.Job.RunTimeout.Duration)}); err != nil {
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
		// Machines of resumed jobs have sessions again, and are spared.
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
			s.deleteMachine(r.machine, io.Discard)
			return nil, "", err
		}
		defer logFile.Close()
		// Cancelled before this process tracked it: nothing to wait for.
		if cur, err := s.queue.Get(j.ID); err == nil && cur.State != stateRunning {
			s.deleteMachine(r.machine, logFile)
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
	// The reaper deletes it later.
	s.logger.Error("delete worker machine", "machine", m.Name, "id", m.ID, "err", err)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// reap deletes worker machines that no running job owns (left over from a
// crash or a failed delete), and any older than max_age whatever owns it,
// cancelling that job. It runs at startup, before any job, and then
// periodically. (deploy/watchdog.sh enforces max_age too, without the
// server.)
func (s *Server) reap(ctx context.Context) error {
	machines, err := s.prov.List(ctx)
	if err != nil {
		return err
	}
	for _, m := range machines {
		id, err := strconv.ParseInt(strings.TrimPrefix(m.Name, "gnoreplay-job-"), 10, 64)
		s.mu.Lock()
		_, active := s.sessions[id]
		s.mu.Unlock()
		active = active && err == nil
		tooOld := time.Since(m.Created) > s.cfg.DigitalOcean.MaxAge.Duration
		if active && !tooOld {
			continue
		}
		if active {
			s.logger.Error("worker machine exceeded its max age: cancelling its job",
				"machine", m.Name, "created", m.Created, "max_age", s.cfg.DigitalOcean.MaxAge)
			s.cancelCause(id, fmt.Errorf("worker machine %s exceeded its max age (%s) and was deleted",
				m.Name, s.cfg.DigitalOcean.MaxAge))
		}
		if err := s.prov.Delete(ctx, m.ID); err != nil {
			s.logger.Error("reap worker machine", "machine", m.Name, "err", err)
			continue
		}
		s.logger.Info("reaped worker machine", "machine", m.Name, "created", m.Created)
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
