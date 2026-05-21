// Package main is the LeadKart API binary entrypoint.
//
// Composition root per Mat Ryer 2024 "How I write HTTP services in Go
// after 13 years": big positional NewServer constructor, manual
// dependency wiring, returns http.Handler. No DI container.
//
// Scope: REQUEST PATH ONLY. The API host writes integration events to
// the per-module outbox table; it does NOT poll the outbox or run
// subscribers. Event processing lives in cmd/worker — both the
// outbox forwarder and the subscriber router. Production deploys both
// binaries; dev environments run them as separate processes against a
// shared Postgres + Redis pair.
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
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"golang.org/x/sync/errgroup"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	commonemail "github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/platform/breach"
	platformemail "github.com/leadkart/leadkart-go/internal/platform/email"
	"github.com/leadkart/leadkart-go/internal/platform/impersonation"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/platform/cache"
	"github.com/leadkart/leadkart-go/internal/platform/config"
	"github.com/leadkart/leadkart-go/internal/platform/httpmw"
	"github.com/leadkart/leadkart-go/internal/platform/idempotency"
	"github.com/leadkart/leadkart-go/internal/platform/obs"
	"github.com/leadkart/leadkart-go/internal/platform/openapi"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

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

// Health-probe + cache + OTel tunings.
const (
	// healthCheckTimeout — per-checker budget on /ready probes.
	healthCheckTimeout = 2 * time.Second
	// otelShutdownTimeout — OpenTelemetry exporter flush + close.
	otelShutdownTimeout = 10 * time.Second
	// redisPingTimeout caps the boot-time Redis reachability check.
	// Distinct from request-time deadlines: a slow PING at boot is a
	// fail-fast crash, not a tail-latency concern.
	redisPingTimeout = 5 * time.Second
	// hybridCacheL1MaxItems sizes ristretto's MaxCost (entry budget).
	// 10k SecurityStamp entries occupy ~1MB on a 36-char value; well
	// under the 256MB-per-pod default container limit.
	hybridCacheL1MaxItems = 10_000

	// IP rate-limit defaults — per-source-IP token bucket on the
	// public listener. Mirrors security.md "Rate limiting on every
	// mutating endpoint": 10 rps sustained / 60-burst handles legitimate
	// browser bursts (page load fans out to a few endpoints) while
	// throttling brute-force credential stuffing.
	apiIPRatePerSecond = 10.0
	apiIPRateBurst     = 60
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

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// OTel SDK install BEFORE the slog logger — obs.NewSlogHandler
	// bridges via otelslog.NewHandler which consults the global
	// LoggerProvider that obs.Setup installs. Wrong order = otelslog
	// binds to the no-op provider + every log record after the bind
	// is dropped from OTLP output even after Setup runs.
	otelShutdown, err := obs.Setup(ctx, cfg.OTel)
	if err != nil {
		return fmt.Errorf("obs: %w", err)
	}

	logger := slog.New(obs.NewSlogHandler(
		slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		cfg.OTel.ServiceName,
	))
	slog.SetDefault(logger)

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			logger.ErrorContext(ctx, "obs shutdown", "err", err)
		}
	}()

	pool, err := pg.NewPool(ctx, cfg.Postgres.DSN, pg.PoolConfig{
		// Production keeps query parameters OFF traces (PII /
		// secret material in identity.persons +
		// refresh_token_families bound args). Per OTel semconv
		// §db.statement.parameters: "MUST NOT be captured by
		// default."
		IncludeQueryParameters: false,
	})
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgxpool ping: %w", err)
	}
	logger.InfoContext(ctx, "postgres connected")

	// Redis client is the single broker for the HybridCache L2 layer
	// (SecurityStamp + future per-resource facades) AND will host the
	// JWT blacklist + impersonation session store + idempotency inbox
	// in v0.3+. Singleton per ADR 0015 + audit-checklist.md §12b
	// "Redis singleton rule" — never per-request, defeats pooling.
	// MaintNotificationsConfig.Mode=Disabled opts out of go-redis 9.19's
	// CLIENT MAINT_NOTIFICATIONS feature (a Redis Enterprise / ElastiCache
	// upgrade-coordination protocol we do not deploy against). The default
	// "auto" mode unconditionally spawns a CircuitBreakerManager goroutine
	// that goleak flags + that we have no use for. Per Redis docs §
	// "Client maintenance notifications" — opt-out for non-Enterprise
	// deployments is the canonical posture.
	redisCli := redis.NewClient(&redis.Options{
		Addr:                     cfg.Redis.Addr,
		Password:                 cfg.Redis.Password,
		DB:                       cfg.Redis.DB,
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
	})
	defer func() { _ = redisCli.Close() }()
	pingCtx, pingCancel := context.WithTimeout(ctx, redisPingTimeout)
	if err := redisCli.Ping(pingCtx).Err(); err != nil {
		pingCancel()
		return fmt.Errorf("redis ping %s: %w", cfg.Redis.Addr, err)
	}
	pingCancel()
	logger.InfoContext(ctx, "redis connected", "addr", cfg.Redis.Addr)

	hybridCache, err := cache.New(cache.Config{
		L1MaxItems: hybridCacheL1MaxItems,
		L2:         redisCli,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("hybrid cache: %w", err)
	}
	defer hybridCache.Close()

	wiring, err := buildIdentityApp(pool, hybridCache, cfg, time.Now)
	if err != nil {
		return fmt.Errorf("build identity app: %w", err)
	}

	// NOTE: outbox forwarder + messaging.Router + subscribers.Register
	// live in cmd/worker — see that binary's package doc. The API host
	// only writes integration events (via the per-handler outbox writes
	// inside each command handler); it does NOT poll the outbox or run
	// subscribers. Production deploys both binaries.

	// Three-endpoint health split lives on the admin listener — public
	// API never carries /alive|/ready|/health (per audit-checklist.md
	// §12: probes excluded from public-facing caches).
	health := obs.NewHealth([]obs.HealthChecker{
		obs.HealthCheckerFunc{N: "postgres", Fn: pool.Ping},
		obs.HealthCheckerFunc{N: "redis", Fn: func(ctx context.Context) error {
			return redisCli.Ping(ctx).Err()
		}},
	}, healthCheckTimeout)
	adminSrv := obs.NewAdminServer(cfg.Listen.Admin, health)

	// Canonical public middleware chain (per httpmw doc):
	//   correlation → requestlog → recover → ip-ratelimit → idempotency
	// Per-route auth (RequireFreshStamp) lives inside the mux that
	// PublicChain wraps — auth must run after IP rate-limiting (an
	// unauthenticated brute-force attempt should hit the limiter
	// before we burn cycles on JWT verification) but before
	// idempotency cache lookups (which are tenant-scoped).
	// PostgresStore is durable across restarts + safe across replicas.
	// InMemoryStore (the previous default) loses every record on
	// rollout, defeating the idempotency contract during deploys.
	mwChain := httpmw.PublicChain(httpmw.PublicChainConfig{
		Logger:           logger,
		IdempotencyStore: idempotency.NewPostgresStore(pool),
		Now:              time.Now,
		IPRateLimit: httpmw.LimiterConfig{
			RatePerSecond: apiIPRatePerSecond,
			Burst:          apiIPRateBurst,
		},
	})
	publicHandler := otelhttp.NewHandler(
		mwChain(newServer(logger, wiring.App, wiring.Issuer, wiring.StampValidator)),
		"leadkart-api",
	)
	srv := &http.Server{
		Addr:              cfg.Listen.API,
		Handler:           publicHandler,
		ReadHeaderTimeout: apiReadHeaderTimeout,
		ReadTimeout:       apiReadTimeout,
		WriteTimeout:      apiWriteTimeout,
		IdleTimeout:       apiIdleTimeout,
	}

	// errgroup orchestrates the long-running goroutines (admin server,
	// public API server, shutdown coordinator). First-error-cancels-rest
	// semantics replace the manual select/errCh + workers.Wait
	// coordination. Outbox forwarder + subscriber router live in
	// cmd/worker.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("admin listening", "addr", cfg.Listen.Admin)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("admin: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		logger.Info("api listening", "addr", cfg.Listen.API)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		<-gctx.Done()
		logger.Info("api shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), apiShutdownTimeout)
		defer cancel()
		_ = adminSrv.Shutdown(shutdownCtx)
		return srv.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
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
// verifier + validator gate authenticated routes. Both must be non-nil
// for the auth-route block to register; the test that only asserts
// probe-route absence on the public mux passes (nil, nil).
func newServer(log *slog.Logger, identityApp app.Application, verifier authn.Verifier, validator authn.StampValidator) http.Handler {
	mux := http.NewServeMux()
	addRootHelpers(mux)
	ports.AddRoutes(mux, log, identityApp, verifier, validator)
	return mux
}

// addRootHelpers registers humane handlers for the cross-cutting URLs
// every browser + tooling client hits unprompted (root + favicon + spec
// + docs UI). Not domain-owned — lives in the composition root per
// Mat Ryer "the host owns URL structure decisions" canon.
//
//   - GET /              → 302 redirect to /docs (Scalar UI is the
//                          discoverable entrypoint for humans + AI)
//   - GET /favicon.ico   → 204 No Content (Stripe / Auth0 convention —
//                          browsers stop asking after the first 204)
//   - GET /openapi.yaml  → embedded OpenAPI 3.1 spec (ADR 0046)
//   - GET /docs          → Scalar UI HTML page (renders the spec)
//   - GET /docs/         → same handler (trailing-slash tolerance)
func addRootHelpers(mux *http.ServeMux) {
	mux.Handle("GET /{$}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs", http.StatusFound)
	}))
	mux.Handle("GET /favicon.ico", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.Handle("GET /openapi.yaml", openapi.SpecHandler())
	mux.Handle("GET /docs", openapi.ScalarHandler())
	mux.Handle("GET /docs/", openapi.ScalarHandler())
}

// identityWiring groups the Identity composition outputs that main()
// threads into the HTTP server (validator, app, issuer) AND the
// subscriber router (families repo + cache for invalidation hookup
// landing in the next commit).
//
// Returned from [buildIdentityApp] so test fixtures + production share
// the same construction path; tests substitute miniredis-backed
// HybridCache to exercise the full stack.
type identityWiring struct {
	App            app.Application
	Issuer         *jwt.Issuer
	StampCache     *adapters.SecurityStampCache
	StampValidator *adapters.SecurityStampValidator
	Families       *adapters.RefreshTokenFamilyRepository
	Persons        *adapters.PersonRepository
}

// ----- Identity wiring -------------------------------------------------------

// buildIdentityApp wires the Identity Application from a pgxpool +
// HybridCache + config + clock. Extracted from run() so tests construct
// an Application backed by a testcontainers pool + miniredis without
// going through the env-var config path.
//
// Returns an [identityWiring] carrying every output the composition
// root needs: Application (HTTP handler graph), Issuer (Verifier for
// the authn middleware), StampCache + StampValidator (route freshness
// gate + subscriber-side invalidation), Families (subscriber
// dependency).
func buildIdentityApp(pool *pgxpool.Pool, hybridCache *cache.HybridCache, cfg config.AppConfig, now func() time.Time) (identityWiring, error) {
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	roles := adapters.NewRoleRepository(pool, tx)
	authRouter := adapters.NewAuthRouterPG(pool, tx)
	permResolver := permissions.NewResolver(memberships, roles)

	stampCache := adapters.NewSecurityStampCache(hybridCache, persons)
	stampValidator := adapters.NewSecurityStampValidator(stampCache)

	previous := make([]jwt.SigningKey, len(cfg.JWT.PreviousKeys))
	for i, p := range cfg.JWT.PreviousKeys {
		previous[i] = jwt.SigningKey{KeyID: p.KeyID, Secret: []byte(p.SigningKey)}
	}
	issuer, err := jwt.NewIssuer(jwt.SigningKey{
		KeyID:  cfg.JWT.KeyID,
		Secret: []byte(cfg.JWT.SigningKey),
	}, previous, now)
	if err != nil {
		return identityWiring{}, fmt.Errorf("jwt issuer: %w", err)
	}

	dummyHash, err := argon2.Hash("dummy-for-timing-flatten")
	if err != nil {
		return identityWiring{}, fmt.Errorf("dummy hash: %w", err)
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
		return identityWiring{}, fmt.Errorf("no-reply email address: %w", err)
	}

	// Impersonation session store. v0.2 ships in-memory (single-
	// process / integration-test fit); production multi-replica
	// drops in a Redis-backed implementation behind the same
	// [impersonation.Store] interface — composition root change only.
	impersonationStore := impersonation.NewInMemoryStore(now)

	return identityWiring{
		Issuer:         issuer,
		StampCache:     stampCache,
		StampValidator: stampValidator,
		Families:       families,
		Persons:        persons,
		App: app.Application{
		Commands: app.Commands{
			RegisterTenant:       command.NewRegisterTenantHandler(tx, tenants, persons, memberships, roles),
			Login:                command.NewLoginHandler(authRouter, families, tenants, permResolver, issuer, now, cfg.Refresh.AbsoluteTTL, dummyHash),
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
			SuspendTenant:                  command.NewSuspendTenantHandler(tenants, memberships),
			ActivateTenant:                 command.NewActivateTenantHandler(tenants),
			MarkTenantForDeletion:          command.NewMarkTenantForDeletionHandler(tenants, memberships),
			RestoreTenant:                  command.NewRestoreTenantHandler(tenants),

			UpdateUserProfile:              command.NewUpdateUserProfileHandler(memberships),
			DeactivateUser:                 command.NewDeactivateUserHandler(memberships),
			ReactivateUser:                 command.NewReactivateUserHandler(memberships),
			AssignUserRole:                 command.NewAssignUserRoleHandler(memberships),
			RevokeUserRole:                 command.NewRevokeUserRoleHandler(memberships),
			ReplaceUserPermissionOverrides: command.NewReplaceUserPermissionOverridesHandler(memberships),
			AssignUserManager:              command.NewAssignUserManagerHandler(memberships),
			RemoveUserManager:              command.NewRemoveUserManagerHandler(memberships),
			CreateUser:                     command.NewCreateUserHandler(tx, persons, memberships),
			AnonymiseUser:                  command.NewAnonymiseUserHandler(memberships, persons),

			CreateRole:             command.NewCreateRoleHandler(roles),
			UpdateRole:             command.NewUpdateRoleHandler(roles),
			DeleteRole:             command.NewDeleteRoleHandler(roles),
			ReplaceRolePermissions: command.NewReplaceRolePermissionsHandler(roles),
			GrantRolePermission:    command.NewGrantRolePermissionHandler(roles),
			RevokeRolePermission:   command.NewRevokeRolePermissionHandler(roles),

			GlobalSuspendPerson:        command.NewGlobalSuspendPersonHandler(persons),
			LiftPersonGlobalSuspension: command.NewLiftPersonGlobalSuspensionHandler(persons),
			AnonymisePerson:            command.NewAnonymisePersonHandler(persons),
			UpdatePersonProfile:        command.NewUpdatePersonProfileHandler(persons),
			HardDeleteTenant:           command.NewHardDeleteTenantHandler(tenants, memberships),

			CreateImpersonationSession: command.NewCreateImpersonationSessionHandler(impersonationStore, tenants, issuer, now),
			EndImpersonationSession:    command.NewEndImpersonationSessionHandler(impersonationStore),
		},
		Queries: app.Queries{
			ListSessions: query.NewListSessionsHandler(families),
			GetCapabilities: query.NewCachedGetCapabilitiesHandler(
				query.NewGetCapabilitiesHandler(persons, memberships, roles),
				hybridCache,
				memberships,
			),
			GetTenant:                 query.NewGetTenantHandler(tenants),
			GetTenantBySlug:           query.NewGetTenantBySlugHandler(tenants),
			GetUser:                   query.NewGetUserHandler(memberships, persons),
			ListUsers:                 query.NewListUsersHandler(memberships, persons),
			ListUsersPaged:            query.NewListUsersPagedHandler(memberships, persons),
			GetRole:                   query.NewGetRoleHandler(roles),
			ListRoles:                 query.NewListRolesHandler(roles),
			GetPerson:                 query.NewGetPersonHandler(persons),
			GetPersonByEmail:          query.NewGetPersonByEmailHandler(persons),
			ListPersonMemberships:     query.NewListPersonMembershipsHandler(memberships, persons),
			ListAllTenants:            query.NewListAllTenantsHandler(tenants),
			ListImpersonationSessions: query.NewListImpersonationSessionsHandler(impersonationStore),
			PlatformStats: query.NewCachedPlatformStatsHandler(
				query.NewPlatformStatsHandler(pool, tx),
				hybridCache,
			),
			Search: query.NewCachedSearchHandler(
				query.NewSearchHandler(pool, tx),
				hybridCache,
			),
			ListAuditEventsByTenant: query.NewListAuditEventsByTenantHandler(pool, tx),
			ListAuditEventsByUser:   query.NewListAuditEventsByUserHandler(pool, tx),
		},
		},
	}, nil
}
