// Package main is the LeadKart API binary entrypoint.
//
// Composition root per Mat Ryer 2024 "How I write HTTP services in Go
// after 13 years": big positional NewServer constructor, manual
// dependency wiring, returns http.Handler. No DI container.
//
// Required environment:
//
//	DATABASE_URL          postgresql DSN (leadkart_app role)
//	JWT_SIGNING_KEY       ≥32-byte HS256 secret (raw bytes ok; hex
//	                      strings must be ≥64 chars)
//	JWT_KEY_ID            kid header value (e.g. "k1")
//	REFRESH_TOKEN_TTL     optional; default 14d (Go duration string)
//	LEADKART_API_ADDR     listen address; default ":8080"
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

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
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
		Addr:              cfg.ListenAddr,
		Handler:           newServer(logger, identityApp),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.ListenAddr)
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

// ----- Config + Identity wiring ---------------------------------------------

type apiConfig struct {
	DatabaseURL    string
	ListenAddr     string
	JWTKeyID       string
	JWTSigningKey  []byte
	RefreshTokenTTL time.Duration
}

func loadConfig() (apiConfig, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return apiConfig{}, errors.New("DATABASE_URL env var required")
	}
	keyID := os.Getenv("JWT_KEY_ID")
	if keyID == "" {
		return apiConfig{}, errors.New("JWT_KEY_ID env var required")
	}
	secret := os.Getenv("JWT_SIGNING_KEY")
	if len(secret) < 32 {
		return apiConfig{}, fmt.Errorf("JWT_SIGNING_KEY must be ≥32 bytes (got %d)", len(secret))
	}
	addr := os.Getenv("LEADKART_API_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	ttl := 14 * 24 * time.Hour
	if v := os.Getenv("REFRESH_TOKEN_TTL"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return apiConfig{}, fmt.Errorf("REFRESH_TOKEN_TTL: %w", err)
		}
		ttl = parsed
	}
	return apiConfig{
		DatabaseURL:     dsn,
		ListenAddr:      addr,
		JWTKeyID:        keyID,
		JWTSigningKey:   []byte(secret),
		RefreshTokenTTL: ttl,
	}, nil
}

// buildIdentityApp wires the Identity Application from a pgxpool +
// config + clock. Extracted from run() so tests construct an
// Application backed by a testcontainers pool without going through
// the env-var config path.
func buildIdentityApp(pool *pgxpool.Pool, cfg apiConfig, now func() time.Time) (app.Application, error) {
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)

	issuer, err := jwt.NewIssuer(jwt.SigningKey{
		KeyID:  cfg.JWTKeyID,
		Secret: cfg.JWTSigningKey,
	}, nil, now)
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
			Login:          command.NewLoginHandler(persons, memberships, families, tenants, issuer, now, cfg.RefreshTokenTTL, dummyHash),
			Refresh:        command.NewRefreshHandler(families, persons, memberships, tenants, issuer, now, cfg.RefreshTokenTTL),
			Logout:         command.NewLogoutHandler(families),
		},
	}, nil
}
