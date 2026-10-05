// Command gnoreplay-server replays a gno.land chain's history with the binary
// of each pushed commit or PR, and reports divergences as a GitHub check run.
// See README.md.
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
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.toml", "path to the config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	key, err := os.ReadFile(cfg.GitHub.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	secret, err := os.ReadFile(cfg.GitHub.WebhookSecretFile)
	if err != nil {
		return fmt.Errorf("read webhook secret: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}

	q, err := openQueue(filepath.Join(cfg.DataDir, "jobs.db"))
	if err != nil {
		return fmt.Errorf("open queue: %w", err)
	}
	defer q.Close()
	if n, err := q.Requeue(); err != nil {
		return err
	} else if n > 0 {
		logger.Info("requeued interrupted jobs", "count", n)
	}

	srv := newServer(cfg, q, newGHApp(cfg.GitHub.AppID, key, cfg.GitHub.CheckName), logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Go(func() { srv.work(ctx) })
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.routes([]byte(strings.TrimSpace(string(secret)))),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()
	logger.Info("listening", "addr", cfg.Listen, "workers", cfg.Workers)
	err = httpSrv.ListenAndServe()
	// Interrupted jobs are requeued on the next start.
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
