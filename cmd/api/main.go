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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	commonemail "github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/platform/breach"
	platformemail "github.com/leadkart/leadkart-go/internal/platform/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/app/service"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/platform/config"
	"github.com/leadkart/leadkart-go/internal/platform/obs"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// IdentityEventsTopic is the Watermill destination used by both the
// outbox forwarder (publish) and downstream module subscribers
// (consume). All identity.* domain events flow through this single
// topic; subscribers route by `event_type` metadata header.
const IdentityEventsTopic = "identity.events"

// HTTP server timeouts — public listener tuning per OWASP API Security
// Top 10 (API4: Unrestricted Resource Consumption). Values match the
// stdlib `net/http` recommended defaults adapted for SaaS-style payloads.
const (
	// apiReadHeaderTimeout caps slowloris-style request-header reads.
	apiReadHeaderTimeout = 5 * time.Second
	// apiReadTimeout is the full-request read budget (headers + body).
	apiReadTimeout = 30 * time.Second
	// apiWriteTimeout is the response-write budget. Streaming endpoints
	// (SSE) override per-handler with `http.ResponseController`.
	apiWriteTimeout = 30 * time.Second
	// apiIdleTimeout is the keep-alive idle connection budget.
	apiIdleTimeout = 120 * time.Second
	// apiShutdownTimeout caps how long the server waits for in-flight
	// requests to finish during graceful shutdown (SIGTERM handling).
	apiShutdownTimeout = 30 * time.Second
)

// Outbox forwarder + health-probe tunings.
const (
	// forwarderPollInterval — how often the forwarder polls the outbox
	// for unforwarded rows when the previous poll returned nothing.
	forwarderPollInterval = time.Second
	// forwarderRetryInterval — backoff after a publish failure before
	// retrying the same row.
	forwarderRetryInterval = 50 * time.Millisecond
	// healthCheckTimeout — per-checker budget on /ready probes.
	healthCheckTimeout = 2 * time.Second
	// otelShutdownTimeout — OpenTelemetry exporter flush + close.
	otelShutdownTimeout = 10 * time.Second
)

func main() {
	// Distroless container HEALTHCHECK probe — chainguard/static has no
	// shell + no wget/curl, so the binary itself becomes the probe per
	// chainguard's canonical "single-binary healthcheck" pattern
	// (chainguard.dev/unchained/minimal-container-images-best-practices).
	// Hits the admin listener's /alive endpoint (the public listener
	// never carries probes per audit-checklist.md §12).
	if len(os.Args) >= 2 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		if err := healthcheck(); err != nil {
			fmt.Fprintf(os.Stderr, "leadkart-api healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(context.Background(), os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "leadkart-api: %v\n", err)
		os.Exit(1)
	}
}

// healthcheckTimeout caps the probe HTTP call. K8s default liveness
// probe timeout is 1s; we give 3s to absorb slow GC pauses without
// false-failing under load.
const healthcheckTimeout = 3 * time.Second

// healthcheck probes the admin listener's /alive endpoint. Returns nil
// on HTTP 200, error otherwise. Reads LEADKART_LISTEN__ADMIN to discover
// where the admin listener is bound — same env var the runtime config
// loader consumes, so probe + listener can never disagree.
func healthcheck() error {
	addr := os.Getenv("LEADKART_LISTEN__ADMIN")
	if addr == "" {
		addr = ":9090"
	}
	if !strings.HasPrefix(addr, ":") {
		// Allow `9090` shorthand alongside `:9090` — some PaaS hosts
		// surface ports without the leading colon.
		addr = ":" + addr
	}
	url := "http://127.0.0.1" + addr + "/alive"

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()

	// Loopback-only target — host is hardcoded "127.0.0.1", only the
	// port is env-derived (LEADKART_LISTEN__ADMIN, default ":9090").
	// gosec's taint analysis can't see through the string concat that
	// the host literal pins to localhost. SSRF is impossible: an
	// attacker who controls LEADKART_LISTEN__ADMIN can at most redirect
	// the probe to a different loopback port in the same container,
	// which has no privilege impact (no other listeners are reachable).
	// Annotation lives on NewRequestWithContext (where url enters the
	// http.Request) AND on Do (where gosec re-checks the request).
	//nolint:gosec // G107: loopback-only host, env-derived port; not SSRF.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	//nolint:gosec // G107: loopback-only host (see NewRequestWithContext above).
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	return nil
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

	otelShutdown, err := obs.Setup(ctx, cfg.OTel)
	if err != nil {
		return fmt.Errorf("obs: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			logger.ErrorContext(ctx, "obs shutdown", "err", err)
		}
	}()

	pool, err := pgxpool.New(ctx, cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgxpool ping: %w", err)
	}
	logger.InfoContext(ctx, "postgres connected")

	identityApp, issuer, err := buildIdentityApp(pool, cfg, time.Now)
	if err != nil {
		return fmt.Errorf("build identity app: %w", err)
	}

	// In-process Watermill GoChannel pub/sub — drains identity.outbox
	// to in-binary subscribers. Production swap to Redis Streams or
	// Kafka happens by replacing this single `Publisher`.
	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(logger))
	defer func() { _ = pubsub.Close() }()

	tx := pg.NewTransactor(pool)
	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, IdentityEventsTopic, 0)

	forwarderCtx, stopForwarder := context.WithCancel(ctx)
	defer stopForwarder()
	var workers sync.WaitGroup
	// Go 1.25 wg.Go captures both the goroutine spawn AND the
	// matching wg.Done() call — strictly cleaner than manual
	// Add(1) + defer Done().
	workers.Go(func() {
		forwarder.Run(forwarderCtx, forwarderPollInterval, forwarderRetryInterval, func(err error) {
			logger.ErrorContext(forwarderCtx, "outbox forwarder", "err", err)
		})
	})

	// Three-endpoint health split lives on the admin listener — public
	// API never carries /alive|/ready|/health (per audit-checklist.md
	// §12: probes excluded from public-facing caches).
	health := obs.NewHealth([]obs.HealthChecker{
		obs.HealthCheckerFunc{N: "postgres", Fn: pool.Ping},
	}, healthCheckTimeout)
	adminSrv := obs.NewAdminServer(cfg.Listen.Admin, health)

	publicHandler := otelhttp.NewHandler(newServer(logger, identityApp, issuer), "leadkart-api")
	srv := &http.Server{
		Addr:              cfg.Listen.API,
		Handler:           publicHandler,
		ReadHeaderTimeout: apiReadHeaderTimeout,
		ReadTimeout:       apiReadTimeout,
		WriteTimeout:      apiWriteTimeout,
		IdleTimeout:       apiIdleTimeout,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("admin listening", "addr", cfg.Listen.Admin)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin: %w", err)
			return
		}
	}()
	go func() {
		logger.Info("api listening", "addr", cfg.Listen.API)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api: %w", err)
			return
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("api shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), apiShutdownTimeout)
		defer cancel()
		stopForwarder()
		workers.Wait()
		_ = adminSrv.Shutdown(shutdownCtx)
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		stopForwarder()
		workers.Wait()
		return err
	}
}

// newServer builds the public HTTP handler tree per Mat Ryer 2024.
//
// Carries business endpoints ONLY. Probes (/alive, /ready, /health) +
// pprof live on the admin listener (see [obs.NewAdminServer]).
//
// All dependencies arrive pre-built — main() owns wiring, this owns
// route registration. Tests construct identityApp with fakes + pass
// it directly.
//
// verifier gates authenticated routes (currently change-password). Pass
// nil to skip wiring those routes — useful for the unit test that only
// asserts probe-route absence on the public mux.
func newServer(log *slog.Logger, identityApp app.Application, verifier authn.Verifier) http.Handler {
	mux := http.NewServeMux()
	ports.AddRoutes(mux, log, identityApp, verifier)
	return mux
}

// ----- Identity wiring -------------------------------------------------------

// buildIdentityApp wires the Identity Application from a pgxpool +
// config + clock. Extracted from run() so tests construct an
// Application backed by a testcontainers pool without going through
// the env-var config path.
//
// Returns the issuer alongside the Application so the caller can pass
// it to [newServer] as the [authn.Verifier]; the issuer's Verify method
// is what gates authenticated routes (change-password and onward).
func buildIdentityApp(pool *pgxpool.Pool, cfg config.AppConfig, now func() time.Time) (app.Application, *jwt.Issuer, error) {
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	roles := adapters.NewRoleRepository(pool, tx)
	onboarding := service.NewTenantOnboardingService(tx, tenants, persons, memberships, roles)
	permResolver := permissions.NewResolver(memberships, roles)

	previous := make([]jwt.SigningKey, len(cfg.JWT.PreviousKeys))
	for i, p := range cfg.JWT.PreviousKeys {
		previous[i] = jwt.SigningKey{KeyID: p.KeyID, Secret: []byte(p.SigningKey)}
	}
	issuer, err := jwt.NewIssuer(jwt.SigningKey{
		KeyID:  cfg.JWT.KeyID,
		Secret: []byte(cfg.JWT.SigningKey),
	}, previous, now)
	if err != nil {
		return app.Application{}, nil, fmt.Errorf("jwt issuer: %w", err)
	}

	dummyHash, err := argon2.Hash("dummy-for-timing-flatten")
	if err != nil {
		return app.Application{}, nil, fmt.Errorf("dummy hash: %w", err)
	}

	// Breach checker: offline list seeded with HIBP top-N weakest
	// passwords. Production swap to k-anonymity API per
	// `security.md` "Password breach check" is a one-line
	// constructor change — all consumers depend on the
	// [breach.Checker] interface, not the concrete impl.
	breachChecker := breach.NewOfflineList()

	// Email gateway. v0.2 wires the in-memory Recorder so the
	// password-reset / email-change flows persist their pending
	// state and emit the integration event but skip the actual
	// SMTP/SES/Msg91 round-trip. Local dev + integration tests use
	// the recorded messages to assert wire-shape. v0.3 swaps in a
	// real provider via the [email.Gateway] interface — composition
	// root change only.
	emailGateway := platformemail.NewRecorder(now)
	noReplyAddress, err := commonemail.New("no-reply@leadkart.local")
	if err != nil {
		return app.Application{}, nil, fmt.Errorf("no-reply email address: %w", err)
	}

	return app.Application{
		Commands: app.Commands{
			RegisterTenant:       command.NewRegisterTenantHandler(onboarding),
			Login:                command.NewLoginHandler(persons, memberships, families, tenants, permResolver, issuer, now, cfg.Refresh.AbsoluteTTL, dummyHash),
			Refresh:              command.NewRefreshHandler(families, persons, memberships, tenants, permResolver, issuer, now, cfg.Refresh.AbsoluteTTL),
			Logout:               command.NewLogoutHandler(families),
			ChangePassword:       command.NewChangePasswordHandler(persons, breachChecker),
			RevokeSession:        command.NewRevokeSessionHandler(families),
			RevokeAllSessions:    command.NewRevokeAllSessionsHandler(families),
			RequestPasswordReset: command.NewRequestPasswordResetHandler(persons, emailGateway, noReplyAddress),
			ConfirmPasswordReset: command.NewConfirmPasswordResetHandler(persons, breachChecker),
			RequestEmailChange:   command.NewRequestEmailChangeHandler(persons, emailGateway, noReplyAddress),
			ConfirmEmailChange:   command.NewConfirmEmailChangeHandler(persons),

			UpdateTenantProfile:            command.NewUpdateTenantProfileHandler(tenants),
			UpdateTenantStatutory:          command.NewUpdateTenantStatutoryHandler(tenants),
			UpdateTenantAdminContact:       command.NewUpdateTenantAdminContactHandler(tenants),
			UpdateTenantSettings:           command.NewUpdateTenantSettingsHandler(tenants),
			UpdateTenantDisplayPreferences: command.NewUpdateTenantDisplayPreferencesHandler(tenants),
			SuspendTenant:                  command.NewSuspendTenantHandler(tenants),
			ActivateTenant:                 command.NewActivateTenantHandler(tenants),
			MarkTenantForDeletion:          command.NewMarkTenantForDeletionHandler(tenants),
			RestoreTenant:                  command.NewRestoreTenantHandler(tenants),

			UpdateUserProfile: command.NewUpdateUserProfileHandler(memberships),
			DeactivateUser:    command.NewDeactivateUserHandler(memberships),
			ReactivateUser:    command.NewReactivateUserHandler(memberships),
		},
		Queries: app.Queries{
			ListSessions: query.NewListSessionsHandler(families),
			GetTenant:    query.NewGetTenantHandler(tenants),
			GetUser:      query.NewGetUserHandler(memberships, persons),
			ListUsers:    query.NewListUsersHandler(memberships, persons),
		},
	}, issuer, nil
}
