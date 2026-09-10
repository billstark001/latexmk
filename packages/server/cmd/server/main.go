package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/auth"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/httpapi"
	"github.com/billstark001/latexmk/packages/server/internal/jobs"
	"github.com/billstark001/latexmk/packages/server/internal/metadata"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/store"
)

var (
	version   = "0.2.0"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var db *store.Postgres
	if cfg.DatabaseURL != "" {
		db, err = store.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			logger.Error("database initialization failed", "error", err)
			os.Exit(2)
		}
		defer db.Close()
	}

	meta := metadata.Collect(cfg, metadata.BuildInfo{Version: version, Commit: commit, BuildDate: buildDate})
	if err := metadata.ValidateToolchain(meta, cfg); err != nil {
		logger.Error("toolchain validation failed", "error", err)
		os.Exit(2)
	}
	runner := compile.NewRunner(cfg)
	authManager := auth.New(cfg, db)
	projects, err := project.New(cfg, db)
	if err != nil {
		logger.Error("project storage initialization failed", "error", err)
		os.Exit(2)
	}
	queue := jobs.New(cfg, meta, runner, projects, db, logger)
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	projects.Start(workerCtx, logger)
	queue.Start(workerCtx)
	apiServer := httpapi.New(cfg, meta, runner, authManager, db, projects, queue, logger)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info(
			"server starting",
			"addr",
			cfg.Addr,
			"version",
			version,
			"profile",
			cfg.ImageProfile,
			"auth_mode",
			cfg.AuthMode,
		)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-signalCtx.Done():
		logger.Info("shutdown requested")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
		return
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	stopWorkers()
	waitErr := queue.Wait(shutdownCtx)
	if shutdownErr != nil {
		logger.Error("graceful HTTP shutdown failed", "error", shutdownErr)
	}
	if waitErr != nil {
		logger.Error("compile workers did not stop before the shutdown deadline", "error", waitErr)
	}
	if shutdownErr != nil || waitErr != nil {
		_ = httpServer.Close()
		os.Exit(1)
	}
	logger.Info("server stopped")
}
