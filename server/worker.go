package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	cfg    *Config
	queue  *Queue
	gh     GitHub
	logger *slog.Logger
	// Sign-in with GitHub, for repos without public reports (see auth.go):
	// sessionKey signs the session cookies, and none means nobody is signed
	// in.
	sessionKey  []byte
	oauthSecret string
	oauth       oauthEndpoints
	viewers     viewerLists
	// prov, when set, runs replays on worker machines (see remote.go);
	// otherwise they run locally in the job sandboxes.
	prov Provisioner

	// wake is signaled when a job is enqueued.
	wake chan struct{}

	gitMu sync.Mutex // serializes git operations on the mirrors

	mu       sync.Mutex
	running  map[int64]context.CancelCauseFunc
	sessions map[int64]*session // remote jobs waiting for their worker
}

func newServer(cfg *Config, q *Queue, gh GitHub, logger *slog.Logger) *Server {
	return &Server{
		cfg: cfg, queue: q, gh: gh, logger: logger, oauth: githubOAuth,
		wake: make(chan struct{}, 1), running: map[int64]context.CancelCauseFunc{}, sessions: map[int64]*session{},
	}
}

func (s *Server) reportsDir() string { return filepath.Join(s.cfg.DataDir, "reports") }

// enqueue queues j and cancels the jobs it supersedes.
func (s *Server) enqueue(j *Job) error {
	superseded, err := s.queue.Enqueue(j)
	if err != nil {
		return err
	}
	s.logger.Info("enqueued", "job", j.ID, "key", j.Key, "sha", j.SHA, "priority", j.Priority)
	for _, old := range superseded {
		s.cancel(old.ID)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func shortSHA(sha string) string { return sha[:min(len(sha), 9)] }

var errSuperseded = errors.New("superseded by a newer push, or the PR was closed")

// errShutdown cancels the server's context when it stops: jobs on worker
// machines are left running, and resumed at the next start; others run again.
var errShutdown = errors.New("server stopping")

func shuttingDown(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errShutdown) }

// cancel stops a running job, superseded or closed: the queue already
// marked it so.
func (s *Server) cancel(id int64) { s.cancelCause(id, errSuperseded) }

// cancelCause stops a running job for cause, recorded as its failure unless
// the queue already marked it otherwise.
func (s *Server) cancelCause(id int64, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.running[id]; ok {
		cancel(cause)
	}
}

// work runs jobs until ctx is done: first those a previous process left on
// worker machines, then queued ones.
func (s *Server) work(ctx context.Context, resume <-chan resumable) {
	for r := range resume {
		if ctx.Err() != nil {
			return
		}
		s.resumeJob(ctx, r)
	}
	for ctx.Err() == nil {
		j, err := s.queue.Claim()
		if err != nil {
			s.logger.Error("claim job", "err", err)
		}
		if j == nil {
			select {
			case <-ctx.Done():
			case <-s.wake:
			case <-time.After(30 * time.Second):
			}
			continue
		}
		s.runJob(ctx, j)
	}
}

func (s *Server) runJob(ctx context.Context, j *Job) {
	s.track(ctx, j, func(ctx context.Context, logger *slog.Logger) (*Report, string, error) {
		return s.replay(ctx, j, logger)
	})
}

// track runs a job's replay, then records its result: it can be cancelled
// meanwhile, and a stopping server leaves it running.
func (s *Server) track(ctx context.Context, j *Job, replay func(context.Context, *slog.Logger) (*Report, string, error)) {
	jobCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s.mu.Lock()
	s.running[j.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, j.ID)
		s.mu.Unlock()
	}()

	logger := s.logger.With("job", j.ID, "key", j.Key, "sha", j.SHA)

	// The baseline is read before this job finishes, so a push job compares
	// against the previous commit on its branch.
	base, err := s.queue.Baseline(j.Repo, j.Branch, time.Now())
	if err != nil {
		logger.Error("load baseline", "err", err)
	}

	report, reportPath, err := replay(jobCtx, logger)
	switch {
	case err == nil:
	case shuttingDown(jobCtx):
		logger.Info("server stopping: replay left for the next start")
		return
	case jobCtx.Err() != nil:
		// Recorded as failed, unless superseded or closed: the queue already
		// marked those, and Finish leaves them alone.
		cause := context.Cause(jobCtx)
		recorded, ferr := s.queue.Finish(j.ID, Job{State: stateFailed, Error: cause.Error()})
		if ferr != nil {
			logger.Error("finish job", "err", ferr)
		}
		logger.Info("replay cancelled", "cause", cause, "recorded", recorded)
		return
	default:
		logger.Error("replay failed", "err", err)
		if _, ferr := s.queue.Finish(j.ID, Job{State: stateFailed, Error: err.Error()}); ferr != nil {
			logger.Error("finish job", "err", ferr)
		}
		return
	}

	out, c, err := s.renderJob(j, report, reportPath, base)
	if err != nil {
		logger.Error("write job page", "err", err)
	}
	recorded, err := s.queue.Finish(j.ID, Job{
		State: stateDone, ReportPath: reportPath, Outcome: out.Outcome, Summary: out.Summary, BaseID: baseID(base),
	})
	if err != nil || !recorded {
		logger.Info("result discarded", "recorded", recorded, "err", err)
		return
	}
	logger.Info("replay done", "outcome", out.Outcome, "new", len(c.New), "inherited", len(c.Inherited), "fixed", len(c.Fixed))
}

// renderJob classifies a job's report against its base job's, and writes the
// job page's body next to the report.
func (s *Server) renderJob(j *Job, report *Report, reportPath string, base *Job) (Outcome, Classification, error) {
	var baseReport *Report
	if base != nil {
		var err error
		if baseReport, err = readReport(base.ReportPath); err != nil {
			s.logger.Error("read baseline report", "job", j.ID, "base", base.ID, "err", err)
			base = nil
		}
	}
	c := classify(report, baseReport)
	out := render(j, report, c, s.reportURL(j), base)
	return out, c, os.WriteFile(bodyPath(reportPath), []byte(out.Body), 0o644)
}

func baseID(base *Job) int64 {
	if base == nil {
		return 0
	}
	return base.ID
}

// bodyPath is where a job's rendered page body is stored, next to its report.
func bodyPath(reportPath string) string {
	return strings.TrimSuffix(reportPath, ".json") + ".md"
}

// replay fetches the commit, builds gnoreplay from it and runs it against a
// copy of the golden data dir. It returns the report and where it is stored.
func (s *Server) replay(ctx context.Context, j *Job, logger *slog.Logger) (*Report, string, error) {
	logger.Info("replay started")
	jobDir := s.jobDir(j.ID)
	if err := os.RemoveAll(jobDir); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return nil, "", err
	}
	defer s.removeJobDir(ctx, jobDir)
	logFile, err := os.Create(filepath.Join(jobDir, "job.log"))
	if err != nil {
		return nil, "", err
	}
	defer logFile.Close()
	step := func(name string) { logger.Info("step", "name", name); fmt.Fprintf(logFile, "==> %s\n", name) }

	src := filepath.Join(jobDir, "src")
	if len(s.cfg.Job.CleanupCommand) > 0 {
		defer func() {
			// Not tied to ctx: a cancelled job still needs cleaning up.
			args := sandbox(s.cfg.Job.CleanupCommand, j, src)
			if err := s.run(context.Background(), nil, jobDir, nil, logFile, args[0], args[1:]...); err != nil {
				logger.Error("cleanup", "err", err)
			}
		}()
	}

	// 1. Source.
	step("fetch")
	if err := s.checkoutJob(ctx, j, src, logFile); err != nil {
		return nil, "", fmt.Errorf("checkout: %w", err)
	}

	// 2. Tool: the server's copy, built against the commit's tree. (Not the
	// commit's own contribs/gnoreplay, if it ever has one: the commit is
	// what is being checked.)
	toolDir := filepath.Join(src, "contribs", "gnoreplay")
	if err := os.RemoveAll(toolDir); err != nil {
		return nil, "", err
	}
	if err := copyDir(s.cfg.Job.GnoreplayOverlay, toolDir); err != nil {
		return nil, "", fmt.Errorf("copy gnoreplay: %w", err)
	}

	// 3. Build and replay, here or on a worker machine.
	if err := os.MkdirAll(s.reportsDir(), 0o755); err != nil {
		return nil, "", err
	}
	reportFile := filepath.Join(jobDir, "report.json")
	if s.prov != nil {
		step("replay on a worker machine")
		if err := s.replayRemote(ctx, j, jobDir, logFile); err != nil {
			return nil, "", err
		}
	} else if err := s.replayLocal(ctx, j, jobDir, src, reportFile, logFile, step); err != nil {
		return nil, "", err
	}
	return s.saveReport(j, reportFile)
}

func (s *Server) jobDir(id int64) string { return filepath.Join(s.cfg.DataDir, "jobs", fmt.Sprint(id)) }

// removeJobDir removes a job's dir once it is over, unless job dirs are kept
// or the server is stopping: its worker may still need it at the next start.
func (s *Server) removeJobDir(ctx context.Context, dir string) {
	if !s.cfg.Job.KeepJobDirs && !shuttingDown(ctx) {
		os.RemoveAll(dir)
	}
}

// saveReport moves a job's report from its job dir to the reports dir.
func (s *Server) saveReport(j *Job, reportFile string) (*Report, string, error) {
	reportPath := filepath.Join(s.reportsDir(), fmt.Sprintf("%d.json", j.ID))
	if err := os.Rename(reportFile, reportPath); err != nil {
		return nil, "", err
	}
	report, err := readReport(reportPath)
	return report, reportPath, err
}

// replayLocal builds the tool in src and replays in the local job sandboxes,
// saving the report to reportFile.
func (s *Server) replayLocal(ctx context.Context, j *Job, jobDir, src, reportFile string, logFile io.Writer, step func(string)) error {
	step("build")
	toolDir := filepath.Join(src, "contribs", "gnoreplay")
	bin := filepath.Join(jobDir, "gnoreplay")
	buildCtx, cancel := context.WithTimeout(ctx, s.cfg.Job.BuildTimeout.Duration)
	defer cancel()
	// The tool's go.sum was resolved against another tree. go -C rather than
	// the process's dir: a VM sandbox doesn't inherit it.
	if err := s.run(buildCtx, sandbox(s.cfg.Job.BuildSandbox, j, src), toolDir, nil, logFile, "go", "-C", toolDir, "mod", "tidy"); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	if err := s.run(buildCtx, sandbox(s.cfg.Job.BuildSandbox, j, src), toolDir, nil, logFile, "go", "-C", toolDir, "build", "-o", bin, "."); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// A private copy of the chain data: the replay opens it exclusively.
	step("copy chain data")
	dataDir := filepath.Join(jobDir, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "db"), 0o755); err != nil {
		return err
	}
	for _, name := range []string{"blockstore.db", "state.db"} {
		args := substitute(s.cfg.Job.CopyCommand,
			"{src}", filepath.Join(s.cfg.Chain.GoldenDir, "db", name),
			"{dst}", filepath.Join(dataDir, "db", name))
		if err := s.run(ctx, nil, jobDir, nil, logFile, args[0], args[1:]...); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}

	step("replay")
	runCtx, cancelRun := context.WithTimeout(ctx, s.cfg.Job.RunTimeout.Duration)
	defer cancelRun()
	err := s.run(runCtx, sandbox(s.cfg.Job.RunSandbox, j, src), jobDir, []string{"GNOROOT=" + src}, logFile, bin,
		"--data-dir", dataDir,
		"--genesis", s.cfg.Chain.Genesis,
		"--work-dir", filepath.Join(jobDir, "work"),
		"--max-diffs", "0",
		"--out", reportFile,
		"--log-level", "warn",
	)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		err = nil // diffs found: that's a result, not a failure
	}
	if err != nil {
		return fmt.Errorf("gnoreplay: %w", err)
	}
	return nil
}

// mergeAttempts and mergeRetry bound the wait for GitHub to compute a PR's
// test merge, which it does in the background after a push.
var (
	mergeAttempts = 6
	mergeRetry    = 10 * time.Second
)

// checkoutJob extracts what j replays into dst, and records it on j: the
// pushed commit for a branch. For a PR, GitHub's test merge of the PR into its
// base branch, as CI tests it, so that changes the PR's branch lacks from its
// base don't show as divergences; failing that (e.g. a conflict), the PR's
// head, with a note saying why.
func (s *Server) checkoutJob(ctx context.Context, j *Job, dst string, log io.Writer) error {
	sha, note := j.SHA, ""
	if j.Event == eventPullRequest {
		merge, why := s.testMerge(ctx, j)
		if merge != "" {
			parents, err := s.checkout(ctx, j.Repo, merge, dst, log)
			if err == nil && len(parents) == 2 && parents[1] == j.SHA {
				sha = merge
				note = fmt.Sprintf("this PR merged into %s at %s (GitHub's test merge)", j.Branch, shortSHA(parents[0]))
			} else {
				fmt.Fprintf(log, "test merge %s unusable (parents %v, err %v)\n", merge, parents, err)
				why = "its test merge could not be used"
				if err := os.RemoveAll(dst); err != nil {
					return err
				}
			}
		}
		if note == "" {
			note = fmt.Sprintf("the PR's head as is: %s, so what it lacks from %s may show as divergences", why, j.Branch)
		}
		fmt.Fprintf(log, "replaying %s\n", note)
	}
	if sha == j.SHA {
		if _, err := s.checkout(ctx, j.Repo, sha, dst, log); err != nil {
			return err
		}
	}
	j.ReplaySHA, j.ReplayNote = sha, note
	return s.queue.SetReplay(j.ID, sha, note)
}

// testMerge returns GitHub's test merge of PR job j, or "" and why there is
// none to replay.
func (s *Server) testMerge(ctx context.Context, j *Job) (sha, why string) {
	for attempt := 1; ; attempt++ {
		pr, err := s.gh.Pull(ctx, j.Repo, j.PR)
		switch {
		case err != nil:
			s.logger.Error("read PR", "job", j.ID, "pr", j.PR, "err", err)
			return "", "GitHub could not be asked for its test merge"
		case pr.HeadSHA != j.SHA:
			return "", "it was pushed to since" // and this job superseded
		case pr.Mergeable != nil && *pr.Mergeable && pr.MergeSHA != "":
			return pr.MergeSHA, ""
		case pr.Mergeable != nil:
			return "", "it conflicts with " + j.Branch
		case attempt == mergeAttempts:
			return "", "GitHub had not computed its test merge"
		}
		select {
		case <-ctx.Done():
			return "", "the job was stopped"
		case <-time.After(mergeRetry):
		}
	}
}

// checkout extracts commit sha of repo into dst, fetching it into a per-repo
// bare mirror, and returns its parents.
func (s *Server) checkout(ctx context.Context, repo, sha, dst string, log io.Writer) (parents []string, err error) {
	// Jobs run concurrently and share the mirror: one creating it must not
	// be seen half-initialized by another, and git fetches into the same
	// repo would contend on its locks.
	s.gitMu.Lock()
	defer s.gitMu.Unlock()

	mirror := filepath.Join(s.cfg.DataDir, "mirrors", repo+".git")
	if _, err := os.Stat(mirror); err != nil {
		if err := s.run(ctx, nil, "", nil, log, "git", "init", "--bare", "-q", mirror); err != nil {
			return nil, err
		}
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + s.gh.Token()))
	// The token goes through the environment, never argv (visible in ps).
	env := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + s.cfg.GitHub.GitURL + ".extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + auth,
		"GIT_TERMINAL_PROMPT=0",
	}
	url := s.cfg.GitHub.GitURL + repo + ".git"
	if err := s.run(ctx, nil, "", env, log, "git", "-C", mirror, "fetch", "-q", "--no-tags", "--depth=1", url, sha); err != nil {
		return nil, err
	}
	// The commit object names its parents even in a shallow mirror.
	object, err := exec.CommandContext(ctx, "git", "-C", mirror, "cat-file", "commit", sha).Output()
	if err != nil {
		return nil, fmt.Errorf("read commit %s: %w", sha, err)
	}
	for _, line := range strings.Split(string(object), "\n") {
		if line == "" {
			break // end of the headers
		}
		if p, ok := strings.CutPrefix(line, "parent "); ok {
			parents = append(parents, p)
		}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	tarball := dst + ".tar"
	if err := s.run(ctx, nil, "", nil, log, "git", "-C", mirror, "archive", "--format=tar", "-o", tarball, sha); err != nil {
		return nil, err
	}
	defer os.Remove(tarball)
	return parents, s.run(ctx, nil, "", nil, log, "tar", "-xf", tarball, "-C", dst)
}

// run runs a command with the given sandbox prefix, logging its output. The
// whole process group is killed when ctx is done.
func (s *Server) run(ctx context.Context, sandbox []string, dir string, env []string, log io.Writer, name string, args ...string) error {
	argv := append(append(append([]string{}, sandbox...), name), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(baseEnv(), env...)
	var tail tailBuffer
	cmd.Stdout = io.MultiWriter(log, &tail)
	cmd.Stderr = io.MultiWriter(log, &tail)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	fmt.Fprintf(log, "$ %s\n", strings.Join(argv, " "))
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w\n%s", err, tail.String())
		}
		return nil
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

// baseEnv is the environment jobs inherit: enough for git and go, and none
// of the server's secrets.
func baseEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOFLAGS", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// substitute replaces placeholders (old, new pairs) in each argument.
func substitute(tmpl []string, oldnew ...string) []string {
	r := strings.NewReplacer(oldnew...)
	out := make([]string, len(tmpl))
	for i, a := range tmpl {
		out[i] = r.Replace(a)
	}
	return out
}

// sandbox expands a sandbox prefix (or the cleanup command) for a job.
func sandbox(prefix []string, j *Job, src string) []string {
	return substitute(prefix, "{job}", fmt.Sprint(j.ID), "{src}", src)
}

func copyDir(src, dst string) error {
	// WalkDir does not follow a symlinked root (the overlay usually is one).
	src, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		bz, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, bz, 0o644)
	})
}

// tailBuffer keeps the last few KB written to it.
type tailBuffer struct{ buf bytes.Buffer }

const tailSize = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > 2*tailSize {
		keep := t.buf.Bytes()[t.buf.Len()-tailSize:]
		t.buf = *bytes.NewBuffer(append([]byte(nil), keep...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.buf.Bytes()
	if len(b) > tailSize {
		b = b[len(b)-tailSize:]
	}
	return string(b)
}
