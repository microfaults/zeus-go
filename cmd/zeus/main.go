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

	// HTTP API server.
	server := api.NewServer(api.Deps{
		Workflows: workflows,
		Runs:      runs,
		Datasets:  datasets,
		Attacks:   manager,
		Metrics:   metrics,
		Snapshots: snapshots,
		Broker:    broker,
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

	manager.StopAll()
	logger.Info("zeus stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
