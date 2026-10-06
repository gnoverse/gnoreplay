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
	default:
		fmt.Fprintln(os.Stderr, "usage: gnoreplay-server [-config path] [enqueue <owner/repo> <pr-number|branch>]")
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
	var viewerKey []byte
	if cfg.ViewerKeyFile != "" {
		if viewerKey, err = os.ReadFile(cfg.ViewerKeyFile); err != nil {
			return nil, nil, fmt.Errorf("read viewer key: %w", err)
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, nil, err
	}
	q, err := openQueue(filepath.Join(cfg.DataDir, "jobs.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("open queue: %w", err)
	}
	srv := newServer(cfg, q, gh, logger)
	srv.viewerKey = strings.TrimSpace(string(viewerKey))
	if do := cfg.DigitalOcean; do != nil {
		token, err := os.ReadFile(do.TokenFile)
		if err != nil {
			q.Close()
			return nil, nil, fmt.Errorf("read digitalocean token: %w", err)
		}
		srv.prov = newDOProvisioner(strings.TrimSpace(string(token)), do)
	}
	return srv, func() { q.Close() }, nil
}

func serve(ctx context.Context, configPath string, logger *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	srv, closeFn, err := setup(configPath, logger)
	if err != nil {
		return err
	}
	defer closeFn()
	if n, err := srv.queue.Requeue(); err != nil {
		return err
	} else if n > 0 {
		logger.Info("requeued interrupted jobs", "count", n)
	}

	var wg sync.WaitGroup
	if srv.prov != nil {
		// Interrupted jobs were requeued: no machine is in use yet, so any
		// left over is deleted before new ones are created.
		if err := srv.reap(ctx); err != nil {
			return fmt.Errorf("reap worker machines: %w", err)
		}
		wg.Go(func() { srv.reapLoop(ctx, 5*time.Minute) })
		workerSrv := &http.Server{
			Addr:              srv.cfg.DigitalOcean.Listen,
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
				stop()
			}
		}()
		logger.Info("worker machines", "provider", "digitalocean", "endpoint", srv.cfg.DigitalOcean.Listen)
	}
	for range srv.cfg.Workers {
		wg.Go(func() { srv.work(ctx) })
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
	// Interrupted jobs are requeued on the next start.
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
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
		return srv.enqueueFor(repo, eventPullRequest, pr.Base, pr.Number, pr.HeadSHA)
	}
	sha, err := srv.gh.BranchHead(ctx, repo, target)
	if err != nil {
		return err
	}
	return srv.enqueueFor(repo, eventPush, target, 0, sha)
}
