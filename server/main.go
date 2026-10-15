// Command gnoreplay-server replays a gno.land chain's history with the binary
// of each pushed commit or PR, and serves the txs whose results would change.
// It only reads from GitHub. See README.md.
//
// Usage:
//
//	gnoreplay-server [-config config.toml]                         # serve
//	gnoreplay-server [-config config.toml] enqueue <repo> <pr|branch> # replay one
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.toml", "path to the config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args := flag.Args(); {
	case len(args) == 0:
		err = serve(ctx, *configPath, logger)
	case args[0] == "enqueue" && len(args) == 3:
		err = enqueueOne(ctx, *configPath, logger, args[1], args[2])
	case args[0] == "rerender":
		err = rerender(*configPath, logger, args[1:])
	default:
		fmt.Fprintln(os.Stderr, "usage: gnoreplay-server [-config path] [enqueue <owner/repo> <pr-number|branch> | rerender [job-id...]]")
		os.Exit(2)
	}
	if err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// setup loads the config and opens what both commands need.
func setup(configPath string, logger *slog.Logger) (*Server, func(), error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, err
	}
	token, err := os.ReadFile(cfg.GitHub.TokenFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read token: %w", err)
	}
	gh, err := newTokenClient(strings.TrimSpace(string(token)))
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, nil, err
	}
	q, err := openQueue(filepath.Join(cfg.DataDir, "jobs.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("open queue: %w", err)
	}
	srv := newServer(cfg, q, gh, logger)
	if o := cfg.GitHub.OAuth; o != nil {
		secret, err := os.ReadFile(o.ClientSecretFile)
		if err != nil {
			q.Close()
			return nil, nil, fmt.Errorf("read github oauth secret: %w", err)
		}
		srv.oauthSecret = strings.TrimSpace(string(secret))
		if srv.sessionKey, err = loadSessionKey(filepath.Join(cfg.DataDir, "session.key")); err != nil {
			q.Close()
			return nil, nil, fmt.Errorf("session key: %w", err)
		}
	}
	if h := cfg.Hetzner; h != nil {
		token, err := os.ReadFile(h.TokenFile)
		if err != nil {
			q.Close()
			return nil, nil, fmt.Errorf("read hetzner token: %w", err)
		}
		srv.prov = newWorkerProvisioner(strings.TrimSpace(string(token)), h)
	}
	return srv, func() { q.Close() }, nil
}

func serve(parent context.Context, configPath string, logger *slog.Logger) error {
	// Stopping cancels with errShutdown, which leaves jobs on worker machines
	// running: the next start resumes them.
	ctx, stop := context.WithCancelCause(context.WithoutCancel(parent))
	defer stop(errShutdown)
	defer context.AfterFunc(parent, func() { stop(errShutdown) })()
	srv, closeFn, err := setup(configPath, logger)
	if err != nil {
		return err
	}
	defer closeFn()
	resumed, err := srv.start(ctx)
	if err != nil {
		return err
	}
	resume := make(chan resumable, len(resumed))
	for _, r := range resumed {
		resume <- r
	}
	close(resume)

	var wg sync.WaitGroup
	// The worker endpoint's own failure: reported at the end, so it does not
	// come out as a clean stop (the process must not exit 0 with it down).
	workerErr := make(chan error, 1)
	if srv.prov != nil {
		// Every minute: idle workers are deleted in the last minutes of their
		// billing hour.
		wg.Go(func() { srv.reapLoop(ctx, time.Minute) })
		workerSrv := &http.Server{
			Addr:              srv.cfg.Hetzner.Listen,
			Handler:           srv.workerRoutes(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			<-ctx.Done()
			workerSrv.Close()
		}()
		go func() {
			if err := workerSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				logger.Error("worker endpoint", "err", err)
				workerErr <- err
				stop(errShutdown)
				return
			}
			workerErr <- nil
		}()
		logger.Info("worker machines", "provider", "hetzner", "endpoint", srv.cfg.Hetzner.Listen)
	}
	for range srv.cfg.Workers {
		wg.Go(func() { srv.work(ctx, resume) })
	}
	wg.Go(func() { srv.pollLoop(ctx) })

	httpSrv := &http.Server{
		Addr:              srv.cfg.Listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()
	logger.Info("listening", "addr", srv.cfg.Listen, "workers", srv.cfg.Workers, "poll", srv.cfg.GitHub.PollInterval)
	err = httpSrv.ListenAndServe()
	stop(errShutdown) // if it failed to start
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		if srv.prov != nil {
			if werr := <-workerErr; werr != nil {
				return fmt.Errorf("worker endpoint: %w", werr)
			}
		}
		return nil
	}
	return err
}

// rerender renders the pages of finished jobs again (all of them without
// ids), once rendering changed. Each is compared with the baseline it had.
func rerender(configPath string, logger *slog.Logger, ids []string) error {
	srv, closeFn, err := setup(configPath, logger)
	if err != nil {
		return err
	}
	defer closeFn()
	var jobs []*Job
	if len(ids) == 0 {
		if jobs, err = srv.queue.Recent(nil, math.MaxInt32); err != nil {
			return err
		}
	}
	for _, id := range ids {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return fmt.Errorf("job id %q: %w", id, err)
		}
		j, err := srv.queue.Get(n)
		if err != nil {
			return fmt.Errorf("job %d: %w", n, err)
		}
		jobs = append(jobs, j)
	}
	for _, j := range jobs {
		if j.State != stateDone {
			continue
		}
		var base *Job
		if j.BaseID != 0 {
			base, err = srv.queue.Get(j.BaseID)
		} else {
			// A job rendered without a baseline keeps none: re-derive the
			// one it would have had when it started, not one that finished
			// while it ran, which rerendering must not turn into a result
			// the job never had. Jobs from before started_at was recorded
			// fall back to their finish.
			at := j.StartedAt
			if at.IsZero() {
				at = j.FinishedAt
			}
			base, err = srv.queue.Baseline(j.Repo, j.Branch, at)
		}
		if err != nil {
			return fmt.Errorf("job %d: baseline: %w", j.ID, err)
		}
		report, err := readReport(j.ReportPath)
		if err != nil {
			return fmt.Errorf("job %d: %w", j.ID, err)
		}
		out, _, err := srv.renderJob(j, report, j.ReportPath, base)
		if err != nil {
			return fmt.Errorf("job %d: %w", j.ID, err)
		}
		if err := srv.queue.SetResult(j.ID, out.Outcome, out.Summary, baseID(base)); err != nil {
			return err
		}
		logger.Info("rendered", "job", j.ID, "base", baseID(base), "outcome", out.Outcome, "summary", out.Summary)
	}
	return nil
}

// enqueueOne queues a replay of a PR's or a branch's current head. A running
// server picks it up from the shared queue.
func enqueueOne(ctx context.Context, configPath string, logger *slog.Logger, repo, target string) error {
	srv, closeFn, err := setup(configPath, logger)
	if err != nil {
		return err
	}
	defer closeFn()
	if n, err := strconv.Atoi(target); err == nil {
		pr, err := srv.gh.Pull(ctx, repo, n)
		if err != nil {
			return err
		}
		if err := srv.enqueueFor(repo, eventPullRequest, pr.Base, pr.Number, pr.HeadSHA); err != nil {
			return err
		}
		return srv.queue.SetPRInfo(jobKey(repo, eventPullRequest, pr.Base, pr.Number), pr.Title, pr.Author)
	}
	sha, err := srv.gh.BranchHead(ctx, repo, target)
	if err != nil {
		return err
	}
	return srv.enqueueFor(repo, eventPush, target, 0, sha)
}
