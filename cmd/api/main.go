// Package main is the LeadKart API binary entrypoint.
//
// Composition root per Mat Ryer 2024 "How I write HTTP services in Go after 13 years":
// big positional NewServer constructor, manual dependency wiring, Server returns
// http.Handler. No DI container.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(context.Background(), os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "leadkart-api: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable entrypoint per Mat Ryer 2024 — main() is a thin shim that
// resolves OS-level concerns (stdin/stdout/args/signals) and delegates here.
func run(ctx context.Context, stdout *os.File, _ []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	addr := os.Getenv("LEADKART_API_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newServer(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		logger.Info("api shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// newServer builds the HTTP handler tree.
//
// Per Mat Ryer 2024: route registration in addRoutes; handler factories in
// handlers.go. For the bare /health endpoint the inline pattern is fine.
func newServer() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", handleHealth())
	return mux
}

// handleHealth returns 200 for liveness probes (Kubernetes, ALB, ELB, etc.).
//
// This is a LIVENESS check — process is alive. Readiness (DB + Redis reachable)
// belongs at /ready and is wired separately when adapters land.
func handleHealth() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}
