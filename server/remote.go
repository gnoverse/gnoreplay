package main

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"fmt"
	"io"
	"io/fs"
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

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// session is a remote job waiting for its worker.
type session struct {
	token string
	src   string // the checkout, served as the job's source
	done  chan workerResult
}

type workerResult struct {
	report string // path of the uploaded report, if the replay completed
	log    string // the worker's log, if it failed
}

func machineName(jobID int64) string { return fmt.Sprintf("gnoreplay-job-%d", jobID) }

// replayRemote runs the job on a new machine and saves its report to
// reportFile.
func (s *Server) replayRemote(ctx context.Context, j *Job, src, reportFile string, log io.Writer) error {
	sess := &session{token: rand.Text(), src: src, done: make(chan workerResult, 1)}
	s.mu.Lock()
	s.sessions[j.ID] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, j.ID)
		s.mu.Unlock()
	}()

	var userData strings.Builder
	do := s.cfg.DigitalOcean
	if err := workerTmpl.Execute(&userData, map[string]string{
		"URL": strings.TrimRight(do.URL, "/"), "JobID": strconv.FormatInt(j.ID, 10), "Token": sess.token,
		"WorkDir":   filepath.Join(workerWorkDir, strconv.FormatInt(j.ID, 10)),
		"GoVersion": do.GoVersion, "GoMemLimit": do.GoMemLimit,
	}); err != nil {
		return err
	}
	m, err := s.prov.Create(ctx, machineName(j.ID), userData.String())
	if err != nil {
		return fmt.Errorf("create worker: %w", err)
	}
	fmt.Fprintf(log, "worker machine %s (%s) created\n", m.Name, m.ID)
	// Machines cost money until deleted, whatever happens to the job.
	defer s.deleteMachine(m, log)

	timeout := time.NewTimer(s.cfg.Job.RunTimeout.Duration)
	defer timeout.Stop()
	select {
	case r := <-sess.done:
		if r.report == "" {
			fmt.Fprintf(log, "worker log:\n%s\n", r.log)
			return fmt.Errorf("worker failed:\n%s", tail(r.log, 4<<10))
		}
		return os.Rename(r.report, reportFile)
	case <-ctx.Done():
		return ctx.Err()
	case <-timeout.C:
		return fmt.Errorf("worker did not report within %s", s.cfg.Job.RunTimeout.Duration)
	}
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

// reap deletes worker machines that no running job owns: left over from a
// crash or a failed delete. It runs at startup, before any job, and then
// periodically.
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
		if err == nil && active {
			continue
		}
		if err := s.prov.Delete(ctx, m.ID); err != nil {
			s.logger.Error("reap worker machine", "machine", m.Name, "err", err)
			continue
		}
		s.logger.Info("reaped worker machine", "machine", m.Name)
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
		writeTar(w, map[string]string{".": sess.src})
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
	mux.HandleFunc("POST /worker/{id}/report", s.withSession(func(w http.ResponseWriter, r *http.Request, sess *session, id int64) {
		f, err := os.CreateTemp(s.reportsDir(), fmt.Sprintf("upload-%d-*.json", id))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = io.Copy(f, http.MaxBytesReader(w, r.Body, 1<<30))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(f.Name())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.finishSession(sess, workerResult{report: f.Name()})
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /worker/{id}/fail", s.withSession(func(w http.ResponseWriter, r *http.Request, sess *session, _ int64) {
		bz, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		s.finishSession(sess, workerResult{log: string(bz)})
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

func (s *Server) finishSession(sess *session, r workerResult) {
	select {
	case sess.done <- r:
	default: // already reported
		if r.report != "" {
			os.Remove(r.report)
		}
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
		if sess == nil || subtle.ConstantTimeCompare([]byte(token), []byte(sess.token)) != 1 {
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
