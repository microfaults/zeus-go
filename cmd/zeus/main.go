package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"atropos-go/loadgen/internal/api"
	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/dataset"
	"atropos-go/loadgen/internal/dedup"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/sse"
	"atropos-go/loadgen/internal/stats"
	"atropos-go/loadgen/internal/workflow"
)

func main() {
	addr := envOr("ZEUS_ADDR", ":8080")

	// Wire dependencies.
	workflows := workflow.NewStore()
	runs := run.NewStore()
	datasets := dataset.NewStore()
	manager := attacker.NewManager()
	metrics := stats.NewMetrics()
	snapshots := stats.NewSnapshotStore()
	broker := sse.NewBroker()

	// Register default dedup bypass strategies.
	manager.RegisterBypass("header", &dedup.HeaderMutator{HeaderName: "X-Idempotency-Key"})
	manager.RegisterBypass("query", &dedup.QueryParamMutator{ParamName: "nonce"})

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
		Addr:    addr,
		Handler: server.Handler(),
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start HTTP server.
	go func() {
		log.Printf("zeus: listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("zeus: server error: %v", err)
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	log.Println("zeus: shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("zeus: http shutdown error: %v", err)
	}

	manager.StopAll()
	log.Println("zeus: stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
