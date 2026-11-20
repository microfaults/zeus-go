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

	app := newApp(logger, appOptions{})
	httpServer := app.server

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start HTTP server.
	go func() {
		logger.Info("zeus starting", "addr", httpServer.Addr)
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

	app.launcher.Close()
	app.attacks.StopAll()
	logger.Info("zeus stopped")
}

// appOptions overrides wiring defaults; the zero value is production.
type appOptions struct {
	HTTPTimeouts     api.HTTPTimeouts // zero = api.DefaultHTTPTimeouts
	SSEHeartbeat     time.Duration    // zero = api.DefaultSSEHeartbeat
	SnapshotInterval time.Duration    // zero = the launcher default
}

// app is zeus's wired dependency graph.
type app struct {
	server   *http.Server
	launcher *run.Launcher
	attacks  *attacker.Manager
	runs     *run.Store
	metrics  *stats.Metrics
}

// newApp wires zeus from the environment (ZEUS_ADDR, ZEUS_K6_BIN,
// ZEUS_K6_DIR, ZEUS_SELF_URL).
func newApp(logger *slog.Logger, opts appOptions) *app {
	addr := envOr("ZEUS_ADDR", ":8080")

	// Wire dependencies.
	workflows := workflow.NewStore()
	runs := run.NewStore()
	datasets := dataset.NewStore()
	manager := attacker.NewManager()
	metrics := stats.NewMetrics()
	snapshots := stats.NewSnapshotStore()
	broker := sse.NewBroker()

	// A run's per-run Prometheus series and stats snapshot live exactly as
	// long as its record. Zeus has no automatic run eviction (see AGENTS.md),
	// so in practice that is the process lifetime.
	runs.OnEvict(func(runID string) {
		metrics.PruneRunLabels(runID)
		snapshots.Delete(runID)
	})

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
		SnapshotInterval: opts.SnapshotInterval,
		Logger:           logger,
	}, runs, snapshots, broker, metrics)

	// HTTP API server.
	server := api.NewServer(api.Deps{
		Workflows:    workflows,
		Runs:         runs,
		Datasets:     datasets,
		Attacks:      manager,
		Metrics:      metrics,
		Snapshots:    snapshots,
		Broker:       broker,
		Launcher:     launcher,
		SSEHeartbeat: opts.SSEHeartbeat,
	})

	timeouts := opts.HTTPTimeouts
	if timeouts == (api.HTTPTimeouts{}) {
		timeouts = api.DefaultHTTPTimeouts
	}
	return &app{
		server:   api.NewHTTPServer(addr, server.Handler(), timeouts),
		launcher: launcher,
		attacks:  manager,
		runs:     runs,
		metrics:  metrics,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
