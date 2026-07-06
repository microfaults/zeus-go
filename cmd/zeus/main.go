// @title       Zeus Load Generation API
// @version     1.0
// @description Zeus is the execution plane for faults-lab. Manteion issues
// @description commands to it. This document is the integration contract between
// @description the two. Run lifecycle state machine and cross-cutting concerns
// @description (dataset handshake, error envelope) are documented in
// @description docs/api-contract.md.
// @servers.url            http://localhost:8080/api/v1
// @servers.description    Local dev (HTTP)
// @servers.url            https://localhost:8080/api/v1
// @servers.description    Local dev (HTTPS)
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"atropos-go/loadgen/internal/api"
	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/dataset"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/sse"
	"atropos-go/loadgen/internal/stats"
	"atropos-go/loadgen/internal/workflow"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	addr := envOr("ZEUS_ADDR", ":8080")

	// Wire dependencies.
	workflows := workflow.NewStore()
	runs := run.NewStore()
	datasets := dataset.NewStore()
	manager := attacker.NewManager()
	metrics := stats.NewMetrics()
	snapshots := stats.NewSnapshotStore()
	broker := sse.NewBroker()

	// k6 launcher: executes workflow runs as supervised subprocesses. The
	// engine fetches pool data over HTTP from our own dataset content
	// endpoint (ZEUS_SELF_URL is how the k6 process addresses this server --
	// in-cluster that is the zeus Service DNS, not localhost).
	selfURL := envOr("ZEUS_SELF_URL", "http://localhost"+addr)
	launcher := run.NewLauncher(run.LauncherConfig{
		K6Bin: envOr("ZEUS_K6_BIN", "k6"),
		K6Dir: envOr("ZEUS_K6_DIR", "./k6"),
		DatasetURL: func(datasetID string) string {
			return selfURL + "/api/v1/datasets/" + datasetID + "/content"
		},
		Logger: logger,
	}, runs, snapshots, broker)

	// HTTP API server.
	server := api.NewServer(api.Deps{
		Workflows: workflows,
		Runs:      runs,
		Datasets:  datasets,
		Attacks:   manager,
		Metrics:   metrics,
		Snapshots: snapshots,
		Broker:    broker,
		Launcher:  launcher,
	})

	httpServer := &http.Server{
		Addr:         addr,
		Handler:      server.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start HTTP server.
	go func() {
		logger.Info("zeus starting", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
	}

	launcher.Close()
	manager.StopAll()
	logger.Info("zeus stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
