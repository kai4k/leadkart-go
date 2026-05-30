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
// Required environment (see internal/common/config/AppConfig):
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

	"github.com/leadkart/leadkart-go/internal/common/ids"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/common/obs"
	"github.com/leadkart/leadkart-go/internal/common/openapi"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"

	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventoryapp "github.com/leadkart/leadkart-go/internal/inventory/app"
	inventorycommand "github.com/leadkart/leadkart-go/internal/inventory/app/command"
	inventoryquery "github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	inventoryports "github.com/leadkart/leadkart-go/internal/inventory/ports"

	platformadapters "github.com/leadkart/leadkart-go/internal/platform/adapters"
	platformapp "github.com/leadkart/leadkart-go/internal/platform/app"
	platformcommand "github.com/leadkart/leadkart-go/internal/platform/app/command"
	platformquery "github.com/leadkart/leadkart-go/internal/platform/app/query"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
	platformports "github.com/leadkart/leadkart-go/internal/platform/ports"

	crmadapters "github.com/leadkart/leadkart-go/internal/crm/adapters"
	crmapp "github.com/leadkart/leadkart-go/internal/crm/app"
	crmcommand "github.com/leadkart/leadkart-go/internal/crm/app/command"
	crmquery "github.com/leadkart/leadkart-go/internal/crm/app/query"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	crmports "github.com/leadkart/leadkart-go/internal/crm/ports"
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

	// Platform module wiring (ADR 0059 — Phase 2 Slice 1). Reuses the
	// same pool + transactor as Identity. Platform HTTP routes use
	// identity.authn middleware via the verifier + stamp validator
	// passed from Identity wiring.
	platformWiring := buildPlatformApp(pool, time.Now)

	// Inventory module wiring (ADR 0061 — Phase 2 Slice 1). Same
	// pool + transactor + middleware story as Platform.
	inventoryAppInstance := buildInventoryApp(pool)

	// CRM module wiring (ADR 0060 — Phase 2 Slice 1). Same pattern as
	// Platform: shares the pool + transactor; HTTP routes gate on
	// identity.authn via the shared verifier + stamp validator.
	crmWiring := buildCrmApp(pool, time.Now)

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
			Burst:         apiIPRateBurst,
		},
	})
	publicHandler := otelhttp.NewHandler(
		mwChain(newServer(logger, wiring.App, platformWiring.App, inventoryAppInstance, crmWiring.App, wiring.Issuer, wiring.StampValidator)),
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
func newServer(
	log *slog.Logger,
	identityApp app.Application,
	platformApp platformapp.Application,
	inventoryAppInstance inventoryapp.Application,
	crmApp crmapp.Application,
	verifier authn.Verifier,
	validator authn.StampValidator,
) http.Handler {
	mux := http.NewServeMux()
	addRootHelpers(mux)
	ports.AddRoutes(mux, log, identityApp, verifier, validator)
	platformports.AddRoutes(mux, log, platformApp, verifier, validator)
	inventoryports.AddRoutes(mux, log, inventoryAppInstance, verifier, validator)
	crmports.AddRoutes(mux, log, crmApp, verifier, validator)
	return mux
}

// addRootHelpers registers humane handlers for the cross-cutting URLs
// every browser + tooling client hits unprompted (root + favicon + spec
// + docs UI). Not domain-owned — lives in the composition root per
// Mat Ryer "the host owns URL structure decisions" canon.
//
//   - GET /              → 302 redirect to /docs (Scalar UI is the
//     discoverable entrypoint for humans + AI)
//   - GET /favicon.ico   → 204 No Content (Stripe / Auth0 convention —
//     browsers stop asking after the first 204)
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
	roleHierarchyEdges := adapters.NewRoleHierarchyEdgeRepository(pool, tx)
	permissionRequests := adapters.NewPermissionRequestRepository(pool, tx)
	authRouter := adapters.NewAuthRouterPG(pool, tx)
	permResolver := permissions.NewResolver(memberships, roles, time.Now)

	// Read-side adapters per ADR 00xx boundary discipline (app/query/
	// depends on the interface; concrete sqlc-aware impl lives in
	// adapters/). [audit.Reader] is declared in internal/common/audit/
	// next to its writer counterpart.
	var auditReader audit.Reader = adapters.NewAuditReaderPG(pool, tx)
	var searchIndex query.SearchIndex = adapters.NewSearchIndexPG(pool, tx)
	var statsReader query.PlatformStatsReader = adapters.NewPlatformStatsReaderPG(pool, tx)

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
	// [passwordpolicy.Checker] interface, not the concrete impl.
	breachChecker := adapters.NewOfflinePasswordList()

	// Per ADR 0057 — email delivery moved to cmd/worker subscriber.
	// cmd/api no longer holds an email.Gateway; the command handlers
	// emit the dispatch event onto the outbox + return immediately.
	// The Watermill subscriber (in cmd/worker) drains + sends.
	//
	// Composition-root simplification: no recorder + no no-reply
	// address wiring here; that's the worker's concern.

	// Impersonation session store. v0.2 ships in-memory (single-
	// process / integration-test fit); production multi-replica
	// drops in a Redis-backed implementation behind the same
	// [impersonation.Store] interface — composition root change only.
	impersonationStore := adapters.NewImpersonationInMemoryStore(now)

	// Aggregate-ID factories per the Pure Domain refactor (ADR 0047 +
	// Khorikov §8). Handlers depend on `func() <T>.ID` injected at
	// composition; production wires UUIDv7 via the `ids` package, tests
	// pin deterministic counters. Function-type (not interface) per the
	// Go stdlib pattern (http.HandlerFunc, sort.Slice less-func) and
	// mirrors our `now func() time.Time` injection.
	newTenantID := func() tenant.ID { return tenant.ID(ids.NewV7().String()) }
	newPersonID := func() person.ID { return person.ID(ids.NewV7().String()) }
	newMembershipID := func() membership.ID { return membership.ID(ids.NewV7().String()) }
	newFamilyID := func() refreshtoken.FamilyID { return refreshtoken.FamilyID(ids.NewV7().String()) }
	newPermissionRequestID := func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) }
	newOverrideID := ids.NewV7
	newRoleID := func() role.ID { return role.ID(ids.NewV7().String()) }
	newRoleHierarchyEdgeID := func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) }

	return identityWiring{
		Issuer:         issuer,
		StampCache:     stampCache,
		StampValidator: stampValidator,
		Families:       families,
		Persons:        persons,
		App: app.Application{
			Commands: app.Commands{
				RegisterTenant:       command.NewRegisterTenantHandler(tx, tenants, persons, memberships, roles, now, newTenantID, newPersonID, newMembershipID),
				Login:                command.NewLoginHandler(authRouter, families, tenants, persons, permResolver, issuer, now, cfg.Refresh.AbsoluteTTL, dummyHash, newFamilyID),
				Refresh:              command.NewRefreshHandler(families, persons, memberships, tenants, permResolver, issuer, now, cfg.Refresh.AbsoluteTTL),
				Logout:               command.NewLogoutHandler(families, now),
				ChangePassword:       command.NewChangePasswordHandler(persons, breachChecker, now),
				RevokeSession:        command.NewRevokeSessionHandler(families, now),
				RevokeAllSessions:    command.NewRevokeAllSessionsHandler(families, now),
				RequestPasswordReset: command.NewRequestPasswordResetHandler(persons, now),
				ConfirmPasswordReset: command.NewConfirmPasswordResetHandler(persons, breachChecker, now),
				RequestEmailChange:   command.NewRequestEmailChangeHandler(persons, now),
				ConfirmEmailChange:   command.NewConfirmEmailChangeHandler(persons, now),

				UpdateTenantProfile:            command.NewUpdateTenantProfileHandler(tenants, now),
				UpdateTenantStatutory:          command.NewUpdateTenantStatutoryHandler(tenants, now),
				UpdateTenantAdminContact:       command.NewUpdateTenantAdminContactHandler(tenants, now),
				UpdateTenantSettings:           command.NewUpdateTenantSettingsHandler(tenants, now),
				UpdateTenantDisplayPreferences: command.NewUpdateTenantDisplayPreferencesHandler(tenants, now),
				SuspendTenant:                  command.NewSuspendTenantHandler(tenants, memberships, now),
				ActivateTenant:                 command.NewActivateTenantHandler(tenants, now),
				MarkTenantForDeletion:          command.NewMarkTenantForDeletionHandler(tenants, memberships, now),
				RestoreTenant:                  command.NewRestoreTenantHandler(tenants, now),

				UpdateUserProfile:              command.NewUpdateUserProfileHandler(memberships, now),
				DeactivateUser:                 command.NewDeactivateUserHandler(memberships, now),
				ReactivateUser:                 command.NewReactivateUserHandler(memberships, now),
				AssignUserRole:                 command.NewAssignUserRoleHandler(memberships, now),
				RevokeUserRole:                 command.NewRevokeUserRoleHandler(memberships, now),
				ReplaceUserPermissionOverrides: command.NewReplaceUserPermissionOverridesHandler(memberships, now),
				AssignUserManager:              command.NewAssignUserManagerHandler(memberships, now),
				RemoveUserManager:              command.NewRemoveUserManagerHandler(memberships, now),
				CreateUser:                     command.NewCreateUserHandler(tx, persons, memberships, now, newPersonID, newMembershipID),
				AnonymiseUser:                  command.NewAnonymiseUserHandler(memberships, persons, now),

				CreateRole:             command.NewCreateRoleHandler(roles, roleHierarchyEdges, tx, now, newRoleID, newRoleHierarchyEdgeID),
				UpdateRole:             command.NewUpdateRoleHandler(roles, now),
				DeleteRole:             command.NewDeleteRoleHandler(roles, now),
				ReplaceRolePermissions: command.NewReplaceRolePermissionsHandler(roles, now),
				GrantRolePermission:    command.NewGrantRolePermissionHandler(roles, now),
				RevokeRolePermission:   command.NewRevokeRolePermissionHandler(roles, now),
				SetRoleParent:          command.NewSetRoleParentHandler(roleHierarchyEdges, tx, now, newRoleHierarchyEdgeID), // ADR 0058

				GlobalSuspendPerson:        command.NewGlobalSuspendPersonHandler(persons, now),
				LiftPersonGlobalSuspension: command.NewLiftPersonGlobalSuspensionHandler(persons, now),
				AnonymisePerson:            command.NewAnonymisePersonHandler(persons, now),
				UpdatePersonProfile:        command.NewUpdatePersonProfileHandler(persons, now),
				HardDeleteTenant:           command.NewHardDeleteTenantHandler(tenants, memberships, now),

				CreateImpersonationSession: command.NewCreateImpersonationSessionHandler(impersonationStore, tenants, issuer, now),
				EndImpersonationSession:    command.NewEndImpersonationSessionHandler(impersonationStore),

				// Permission-elevation approval workflow (ADR 0055).
				RequestPermissionElevation: command.NewRequestPermissionElevationHandler(permissionRequests, memberships, now, newPermissionRequestID),
				ApprovePermissionRequest:   command.NewApprovePermissionRequestHandler(permissionRequests, memberships, now, newOverrideID),
				DenyPermissionRequest:      command.NewDenyPermissionRequestHandler(permissionRequests, memberships, now),
				CancelPermissionRequest:    command.NewCancelPermissionRequestHandler(permissionRequests, now),
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
				GetRole:                   query.NewGetRoleHandler(roles, roleHierarchyEdges),
				ListRoles:                 query.NewListRolesHandler(roles, roleHierarchyEdges),
				GetPerson:                 query.NewGetPersonHandler(persons),
				GetPersonByEmail:          query.NewGetPersonByEmailHandler(persons),
				ListPersonMemberships:     query.NewListPersonMembershipsHandler(memberships, persons),
				ListAllTenants:            query.NewListAllTenantsHandler(tenants),
				ListImpersonationSessions: query.NewListImpersonationSessionsHandler(impersonationStore),
				PlatformStats: query.NewCachedPlatformStatsHandler(
					query.NewPlatformStatsHandler(statsReader),
					hybridCache,
				),
				Search: query.NewCachedSearchHandler(
					query.NewSearchHandler(searchIndex),
					hybridCache,
				),
				ListAuditEventsByTenant: query.NewListAuditEventsByTenantHandler(auditReader),
				ListAuditEventsByUser:   query.NewListAuditEventsByUserHandler(auditReader),

				// Permission-elevation approval workflow (ADR 0055).
				GetPermissionRequest:     query.NewGetPermissionRequestHandler(permissionRequests),
				ListMyPermissionRequests: query.NewListMyPermissionRequestsHandler(permissionRequests),
				ListPendingForApprover:   query.NewListPendingForApproverHandler(permissionRequests),
			},
		},
	}, nil
}

// ----- Platform wiring (ADR 0059 — Slice 1) -------------------------------

// platformWiringResult is the output of buildPlatformApp — used by run
// to attach the Platform routes. cmd/worker wires the platform-side
// outbox forwarder + subscriber router via its own buildPlatformWorker.
type platformWiringResult struct {
	App platformapp.Application
}

// buildPlatformApp wires the Platform module per ADR 0059. Reuses the
// same pgxpool + transactor as Identity (single DB, schema-per-module
// per ADR 0001). Returns an Application + the concrete repository
// pointers — main()'s newServer threads the Application into the HTTP
// handler tree.
func buildPlatformApp(pool *pgxpool.Pool, now func() time.Time) platformWiringResult {
	tx := pg.NewTransactor(pool)

	contacts := platformadapters.NewUnverifiedContactRepository(pool, tx)
	calls := platformadapters.NewVerificationCallRepository(pool, tx)
	leads := platformadapters.NewPlatformLeadRepository(pool, tx)
	credits := platformadapters.NewLeadCreditRepository(pool, tx)
	contactReader := platformadapters.NewUnverifiedContactReader(pool, tx)
	outboxEnq := platformadapters.NewOutboxEnqueuer()

	// Platform aggregate-ID factories (Pure Domain refactor — see
	// buildIdentityApp for the canon).
	newContactID := func() unverifiedcontact.ID { return unverifiedcontact.ID(ids.NewV7().String()) }
	newCallID := func() verificationcall.ID { return verificationcall.ID(ids.NewV7().String()) }
	newLeadID := func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) }
	newPurchaseID := func() string { return ids.NewV7().String() }

	return platformWiringResult{
		App: platformapp.Application{
			Commands: platformapp.Commands{
				CreateUnverifiedContact: platformcommand.NewCreateUnverifiedContactHandler(contacts, now, newContactID),
				LogVerificationCall:     platformcommand.NewLogVerificationCallHandler(tx, calls, contacts, now, newCallID),
				VerifyUnverifiedContact: platformcommand.NewVerifyUnverifiedContactHandler(tx, contacts, leads, outboxEnq, now, newLeadID),
				RejectUnverifiedContact: platformcommand.NewRejectUnverifiedContactHandler(contacts, now),
				PurchaseLead:            platformcommand.NewPurchaseLeadHandler(tx, leads, credits, outboxEnq, now, newPurchaseID),
				TopupLeadCredits:        platformcommand.NewTopupLeadCreditsHandler(tx, credits, now),
			},
			Queries: platformapp.Queries{
				ListUnverifiedContacts: platformquery.NewListUnverifiedContactsHandler(contactReader),
				BrowseMarketplace:      platformquery.NewBrowseMarketplaceHandler(leads),
				GetLeadCreditBalance:   platformquery.NewGetLeadCreditBalanceHandler(credits),
			},
		},
	}
}

// ----- Inventory wiring (ADR 0061 — Slice 1) -------------------------------

// buildInventoryApp wires the Inventory Application from a pgxpool.
// Mirror of buildIdentityApp at a smaller scale — single bounded context,
// no JWT issuance / cache concerns.
//
// All inventory adapters share the same pgxpool + Transactor as identity
// per CLAUDE.md "Each module owns its Postgres schema. No cross-schema
// joins" — same connection pool, distinct schemas, distinct outboxes.
func buildInventoryApp(pool *pgxpool.Pool) inventoryapp.Application {
	tx := pg.NewTransactor(pool)
	products := inventoryadapters.NewProductRepository(pool, tx)
	batches := inventoryadapters.NewBatchRepository(pool, tx)
	movements := inventoryadapters.NewStockMovementRepository(pool, tx)

	// Inventory aggregate-ID factories (Pure Domain refactor).
	newProductID := func() product.ID { return product.ID(ids.NewV7().String()) }
	newBatchID := func() batch.ID { return batch.ID(ids.NewV7().String()) }
	newMovementID := func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) }

	return inventoryapp.Application{
		Commands: inventoryapp.Commands{
			CreateProduct:    inventorycommand.NewCreateProductHandler(products, time.Now, newProductID),
			UpdateProduct:    inventorycommand.NewUpdateProductHandler(products, time.Now),
			DeleteProduct:    inventorycommand.NewDeleteProductHandler(products, batches, time.Now),
			AddBatch:         inventorycommand.NewAddBatchHandler(tx, products, batches, time.Now, newBatchID),
			LogStockMovement: inventorycommand.NewLogStockMovementHandler(tx, batches, movements, time.Now, newMovementID),
		},
		Queries: inventoryapp.Queries{
			GetProduct:             inventoryquery.NewGetProductHandler(products),
			ListProductsPage:       inventoryquery.NewListProductsPageHandler(products),
			GetBatch:               inventoryquery.NewGetBatchHandler(batches),
			ListBatchesByProduct:   inventoryquery.NewListBatchesByProductHandler(batches),
			ListBatchMovementsPage: inventoryquery.NewListBatchMovementsPageHandler(movements),
		},
	}
}

// ----- CRM wiring ------------------------------------------------------------

// crmWiringResult is the output of buildCrmApp — used by run to attach
// the CRM routes. The worker side (subscriber + outbox forwarder) lives
// in cmd/worker/main.go.
type crmWiringResult struct {
	App crmapp.Application
}

// buildCrmApp wires the CRM module per ADR 0060. Reuses the same
// pgxpool + transactor as Identity / Platform — single DB,
// schema-per-module per ADR 0001.
func buildCrmApp(pool *pgxpool.Pool, now func() time.Time) crmWiringResult {
	tx := pg.NewTransactor(pool)

	leads := crmadapters.NewCrmLeadRepository(pool, tx)
	calls := crmadapters.NewCallLogRepository(pool, tx)
	history := crmadapters.NewAssignmentHistoryRepository(pool, tx)

	newCrmLeadID := func() crmlead.ID { return crmlead.ID(ids.NewV7().String()) }
	newCallLogID := func() calllog.ID { return calllog.ID(ids.NewV7().String()) }
	newHistoryID := func() assignmenthistory.ID { return assignmenthistory.ID(ids.NewV7().String()) }

	return crmWiringResult{
		App: crmapp.Application{
			Commands: crmapp.Commands{
				IngestPurchasedLead:   crmcommand.NewIngestPurchasedLeadHandler(leads, now, newCrmLeadID),
				AssignLead:            crmcommand.NewAssignLeadHandler(leads, history, tx, now, newHistoryID),
				ChangeLeadStage:       crmcommand.NewChangeLeadStageHandler(leads, now),
				ChangeLeadTemperature: crmcommand.NewChangeLeadTemperatureHandler(leads, now),
				LogCall:               crmcommand.NewLogCallHandler(leads, calls, now, newCallLogID),
				ConvertLead:           crmcommand.NewConvertLeadHandler(leads, now),
				LoseLead:              crmcommand.NewLoseLeadHandler(leads, now),
			},
			Queries: crmapp.Queries{
				GetLead:   crmquery.NewGetLeadHandler(leads),
				ListLeads: crmquery.NewListLeadsHandler(leads),
			},
		},
	}
}
