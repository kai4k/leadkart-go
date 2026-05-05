// Package main is the LeadKart API binary entrypoint.
//
// Composition root per Mat Ryer 2024 "How I write HTTP services in Go
// after 13 years": big positional NewServer constructor, manual
// dependency wiring, returns http.Handler. No DI container.
//
// Required environment (see internal/platform/config/AppConfig):
//
//	LEADKART_POSTGRES__DSN        postgres DSN (leadkart_app role)
//	LEADKART_REDIS__ADDR          redis "host:port" (HybridCache L2 + sessions)
//	LEADKART_JWT__KEY_ID          kid header value (e.g. "k1")
//	LEADKART_JWT__SIGNING_KEY     ≥32-byte HS256 secret
//	LEADKART_LISTEN__API          listen address (default ":8080")
//	LEADKART_LISTEN__ADMIN        pprof + metrics listener (default ":9090")
//	LEADKART_REFRESH__ABSOLUTE_TTL  default 336h (14d)
//	LEADKART_CONFIG_FILE          optional YAML overlay before env
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/platform/config"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// IdentityEventsTopic is the Watermill destination used by both the
// outbox forwarder (publish) and downstream module subscribers
// (consume). All identity.* domain events flow through this single
// topic; subscribers route by `event_type` metadata header.
const IdentityEventsTopic = "identity.events"

func main() {
	if err := run(context.Background(), os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "leadkart-api: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable entrypoint per Mat Ryer 2024 — main() resolves
// OS-level concerns (stdin/stdout/args/signals) and delegates here.
func run(ctx context.Context, stdout *os.File, _ []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgxpool ping: %w", err)
	}
	logger.InfoContext(ctx, "postgres connected")

	identityApp, err := buildIdentityApp(pool, cfg, time.Now)
	if err != nil {
		return fmt.Errorf("build identity app: %w", err)
	}

	// In-process Watermill GoChannel pub/sub — drains identity.outbox
	// to in-binary subscribers. Production swap to Redis Streams or
	// Kafka happens by replacing this single `Publisher`.
	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(logger))
	defer pubsub.Close()

	tx := pg.NewTransactor(pool)
	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, IdentityEventsTopic, 0)

	forwarderCtx, stopForwarder := context.WithCancel(ctx)
	defer stopForwarder()
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		forwarder.Run(forwarderCtx, time.Second, 50*time.Millisecond, func(err error) {
			logger.ErrorContext(forwarderCtx, "outbox forwarder", "err", err)
		})
	}()

	srv := &http.Server{
		Addr:              cfg.Listen.API,
		Handler:           newServer(logger, identityApp),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.Listen.API)
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
		stopForwarder()
		workers.Wait()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		stopForwarder()
		workers.Wait()
		return err
	}
}

// newServer builds the HTTP handler tree per Mat Ryer 2024.
//
// All dependencies arrive pre-built — main() owns wiring, this owns
// route registration. Tests construct identityApp with fakes and pass
// it directly.
func newServer(log *slog.Logger, identityApp app.Application) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", handleHealth())
	ports.AddRoutes(mux, log, identityApp)
	return mux
}

// handleHealth returns 200 for liveness probes (Kubernetes, ALB).
// LIVENESS only — readiness checks (DB+Redis reachable) belong at
// /ready and ship when those adapters land.
func handleHealth() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}

// ----- Identity wiring -------------------------------------------------------

// buildIdentityApp wires the Identity Application from a pgxpool +
// config + clock. Extracted from run() so tests construct an
// Application backed by a testcontainers pool without going through
// the env-var config path.
func buildIdentityApp(pool *pgxpool.Pool, cfg config.AppConfig, now func() time.Time) (app.Application, error) {
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)

	previous := make([]jwt.SigningKey, len(cfg.JWT.PreviousKeys))
	for i, p := range cfg.JWT.PreviousKeys {
		previous[i] = jwt.SigningKey{KeyID: p.KeyID, Secret: []byte(p.SigningKey)}
	}
	issuer, err := jwt.NewIssuer(jwt.SigningKey{
		KeyID:  cfg.JWT.KeyID,
		Secret: []byte(cfg.JWT.SigningKey),
	}, previous, now)
	if err != nil {
		return app.Application{}, fmt.Errorf("jwt issuer: %w", err)
	}

	dummyHash, err := argon2.Hash("dummy-for-timing-flatten")
	if err != nil {
		return app.Application{}, fmt.Errorf("dummy hash: %w", err)
	}

	return app.Application{
		Commands: app.Commands{
			RegisterTenant: command.NewRegisterTenantHandler(tenants, persons, memberships),
			Login:          command.NewLoginHandler(persons, memberships, families, tenants, issuer, now, cfg.Refresh.AbsoluteTTL, dummyHash),
			Refresh:        command.NewRefreshHandler(families, persons, memberships, tenants, issuer, now, cfg.Refresh.AbsoluteTTL),
			Logout:         command.NewLogoutHandler(families),
		},
	}, nil
}
