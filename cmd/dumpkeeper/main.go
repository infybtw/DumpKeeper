package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/config"
	"dumpkeeper/internal/db"
	"dumpkeeper/internal/scheduler"
	"dumpkeeper/internal/storage"
	"dumpkeeper/internal/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}
	for _, dir := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "backups")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Error("create data directory", "dir", dir, "err", err)
			os.Exit(1)
		}
	}

	store, err := db.Open(filepath.Join(cfg.DataDir, "dumpkeeper.db"))
	if err != nil {
		slog.Error("open database", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	local := storage.NewLocal(filepath.Join(cfg.DataDir, "backups"))
	engine := backup.New(store, local)

	sched := scheduler.New(engine.Trigger)
	jobs, err := store.ListJobs()
	if err != nil {
		slog.Error("load jobs", "err", err)
		os.Exit(1)
	}
	for _, j := range jobs {
		sched.Reschedule(j)
	}

	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: web.New(cfg, store, engine, sched).Handler()}
	slog.Info("dumpkeeper listening", "addr", cfg.ListenAddr, "data_dir", cfg.DataDir, "jobs", len(jobs))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErr := make(chan error, 1)
	go func() { serverErr <- httpServer.ListenAndServe() }()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
	}

	slog.Info("shutting down: http, then cron, then in-flight backups")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown", "err", err)
	}
	sched.Stop()
	engine.Wait() // no timeout: interrupting pg_dump risks a corrupt dump
	slog.Info("shutdown complete")
}
