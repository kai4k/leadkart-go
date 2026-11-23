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
//	LEADKART_HTTP__TRUSTED_PROXIES  CIDRs/IPs whose X-Forwarded-For is believed (default empty = none)
//	LEADKART_RATE_LIMIT__<ROUTE>__PER_MINUTE / __BURST  per-route limits (LOGIN, REFRESH,
//	                              PASSWORD_RESET_REQUEST, PASSWORD_RESET, EMAIL_CHANGE_CONFIRM,
//	                              TENANT_REGISTRATION)
//	LEADKART_CONFIG_FILE          optional YAML overlay before env
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
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
	"go.opentelemetry.io/otel"

	"github.com/leadkart/leadkart-go/internal/common/ids"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/common/obs"
	"github.com/leadkart/leadkart-go/internal/common/openapi"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/refdata"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
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
	crmreminder "github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	crmports "github.com/leadkart/leadkart-go/internal/crm/ports"

	tasksadapters "github.com/leadkart/leadkart-go/internal/tasks/adapters"
	tasksapp "github.com/leadkart/leadkart-go/internal/tasks/app"
	taskscommand "github.com/leadkart/leadkart-go/internal/tasks/app/command"
	tasksquery "github.com/leadkart/leadkart-go/internal/tasks/app/query"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
	tasksports "github.com/leadkart/leadkart-go/internal/tasks/ports"

	companysettingsadapters "github.com/leadkart/leadkart-go/internal/companysettings/adapters"
	companysettingsapp "github.com/leadkart/leadkart-go/internal/companysettings/app"
	companysettingscommand "github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	companysettingsquery "github.com/leadkart/leadkart-go/internal/companysettings/app/query"
	companysettingsports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
	dispatchadapters "github.com/leadkart/leadkart-go/internal/dispatch/adapters"
	dispatchapp "github.com/leadkart/leadkart-go/internal/dispatch/app"
	dispatchcommand "github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	dispatchquery "github.com/leadkart/leadkart-go/internal/dispatch/app/query"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"

	ordersadapters "github.com/leadkart/leadkart-go/internal/orders/adapters"
	ordersapp "github.com/leadkart/leadkart-go/internal/orders/app"
	orderscommand "github.com/leadkart/leadkart-go/internal/orders/app/command"
	ordersquery "github.com/leadkart/leadkart-go/internal/orders/app/query"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"

	notificationsadapters "github.com/leadkart/leadkart-go/internal/notifications/adapters"
	notificationsapp "github.com/leadkart/leadkart-go/internal/notifications/app"
	notificationscommand "github.com/leadkart/leadkart-go/internal/notifications/app/command"
	notificationsquery "github.com/leadkart/leadkart-go/internal/notifications/app/query"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	notificationsports "github.com/leadkart/leadkart-go/internal/notifications/ports"
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
	// Fail fast when the database role lacks a privilege the application
	// needs (the grants migration is a no-op when the role did not exist
	// at migration time). One query, run once; names the first missing
	// privilege, never the DSN.
	if err := pg.VerifyApplicationPrivileges(ctx, pool, pg.PrivilegeCheck{Production: cfg.Env == "production", Logger: logger}); err != nil {
		return err
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

	// A dead worker must be visible from here: the API writes the outbox and the
	// phantom-lockout table, only the worker drains and purges them.
	if err := obs.RegisterBacklogGauges(ctx, pool, logger); err != nil {
		return fmt.Errorf("backlog gauges: %w", err)
	}

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
	inventoryAppInstance := buildInventoryApp(pool, hybridCache)

	// CRM module wiring (ADR 0060 — Phase 2 Slice 1). Same pattern as
	// Platform: shares the pool + transactor; HTTP routes gate on
	// identity.authn via the shared verifier + stamp validator.
	crmWiring := buildCrmApp(pool, time.Now)

	// Tasks module wiring (Phase C.2 — BRD §6.8). Same pattern as CRM.
	tasksAppInstance := buildTasksApp(pool, time.Now)

	// Dispatch module wiring (ADR 0063 — BRD §6.6). HTTP read + status
	// transitions; the OrderPacked subscriber lives in cmd/worker.
	dispatchAppInstance := buildDispatchApp(pool, time.Now)
	companySettingsAppInstance := buildCompanySettingsApp(pool, time.Now)

	// Orders module wiring (ADR 0063 — BRD §6.4). Quote→order lifecycle +
	// gapless FY invoice numbering; saga subscribers live in cmd/worker.
	ordersAppInstance := buildOrdersApp(pool, time.Now)
	notificationsAppInstance := buildNotificationsApp(pool, time.Now)

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
	mwChain, err := buildPublicChain(cfg, logger, idempotency.NewPostgresStore(pool), wiring.Issuer, time.Now)
	if err != nil {
		return err
	}
	publicHandler := otelhttp.NewHandler(
		mwChain(newServer(logger, wiring.App, platformWiring.App, inventoryAppInstance, crmWiring.App, tasksAppInstance, dispatchAppInstance, ordersAppInstance, notificationsAppInstance, companySettingsAppInstance, wiring.Issuer, wiring.StampValidator)),
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

// buildPublicChain assembles the public middleware chain from configuration.
// main() and the wiring tests both build the chain here, so a test that drives it
// exercises exactly what production runs: the trusted-proxy list, the global and
// per-route limiters and the verified idempotency scope. now is the clock of the
// idempotency records (and, unless a test overrides it, the limiters).
func buildPublicChain(cfg config.AppConfig, logger *slog.Logger, store idempotency.Store, verifier authn.Verifier, now func() time.Time) (httpmw.Middleware, error) {
	trustedProxies, err := httpmw.ParseTrustedProxies(cfg.HTTP.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}
	effective := make([]string, len(trustedProxies))
	for i, p := range trustedProxies {
		effective[i] = p.String()
	}
	logger.Info("trusted proxies in effect (X-Forwarded-For is read only from these peers)",
		"trusted_proxies", effective, "env", cfg.Env)
	if len(trustedProxies) == 0 {
		logger.Warn("no trusted proxies: X-Forwarded-For is ignored, so behind a BFF all clients share one rate-limit bucket (set LEADKART_HTTP__TRUSTED_PROXIES; production refuses to start without a decision)")
	}
	return httpmw.Chain(argonClientContext(trustedProxies), httpmw.PublicChain(httpmw.PublicChainConfig{
		Logger:           logger,
		IdempotencyStore: store,
		Now:              now,
		IPRateLimit: httpmw.LimiterConfig{
			RatePerSecond: apiIPRatePerSecond,
			Burst:         apiIPRateBurst,
		},
		TrustedProxies:   trustedProxies,
		RouteRateLimits:  routeRateLimits(cfg.RateLimit),
		IdempotencyKeyer: authn.IdempotencyCallerKeyer(verifier),
	})), nil
}

// argonClientContext names the caller (the rate-limit key: the forwarded client,
// IPv6 per /64) in the request context, so the Argon2 gate can bound the password
// work one client has in flight (ADR 0068 D12, fix round 12 item 3).
func argonClientContext(trusted []netip.Prefix) httpmw.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(argon2.WithClient(r.Context(), httpmw.ClientKey(r, trusted))))
		})
	}
}

// routeRateLimits maps the per-route settings onto the exact "METHOD /path"
// patterns the host registers (TestRouteRateLimits_KeysAreRegisteredRoutes pins
// each key to a real route, so a renamed path cannot silently lose its limit).
func routeRateLimits(rl config.RateLimitConfig) httpmw.RouteRateLimits {
	perSecond := func(l config.RouteLimit) httpmw.LimiterConfig {
		return httpmw.LimiterConfig{RatePerSecond: l.PerMinute / 60, Burst: l.Burst}
	}
	return httpmw.RouteRateLimits{
		"POST /api/v1/auth/login":                  perSecond(rl.Login),
		"POST /api/v1/auth/refresh":                perSecond(rl.Refresh),
		"POST /api/v1/auth/request-password-reset": perSecond(rl.PasswordResetRequest),
		"POST /api/v1/auth/reset-password":         perSecond(rl.PasswordReset),
		"POST /api/v1/auth/confirm-email-change":   perSecond(rl.EmailChangeConfirm),
		"POST /api/v1/tenants":                     perSecond(rl.TenantRegistration),
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
// verifier + validator gate authenticated routes. Both must be non-nil
// for the auth-route block to register; the test that only asserts
// probe-route absence on the public mux passes (nil, nil).
func newServer(
	log *slog.Logger,
	identityApp app.Application,
	platformApp platformapp.Application,
	inventoryAppInstance inventoryapp.Application,
	crmApp crmapp.Application,
	tasksApp tasksapp.Application,
	dispatchApp dispatchapp.Application,
	ordersApp ordersapp.Application,
	notificationsApp notificationsapp.Application,
	companySettingsApp companysettingsapp.Application,
	verifier authn.Verifier,
	validator authn.StampValidator,
) http.Handler {
	mux := http.NewServeMux()
	addRootHelpers(mux)
	ports.AddRoutes(mux, log, identityApp, verifier, validator)
	platformports.AddRoutes(mux, log, platformApp, verifier, validator)
	inventoryports.AddRoutes(mux, log, inventoryAppInstance, verifier, validator)
	crmports.AddRoutes(mux, log, crmApp, verifier, validator)
	tasksports.AddRoutes(mux, log, tasksApp, verifier, validator)
	dispatchports.AddRoutes(mux, log, dispatchApp, verifier, validator)
	ordersports.AddRoutes(mux, log, ordersApp, verifier, validator)
	notificationsports.AddRoutes(mux, log, notificationsApp, verifier, validator)
	companysettingsports.AddRoutes(mux, log, companySettingsApp, verifier, validator)
	if verifier == nil {
		return mux
	}
	// ADR 0039 per-request scope selection (ADR 0068 D7): the single place
	// X-Tenant-Id is read, wrapping the whole mux.
	return authn.TenantScope(ports.NewTenantScopeResolver(identityApp).Lookup, log)(mux)
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
	// ArgonGate bounds concurrent Argon2id work for every command that hashes or
	// verifies a password (exposed so a wiring test can saturate it).
	ArgonGate *argon2.Gate
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
	authorityLock := adapters.NewAuthorityLockPG()                                                         // ADR 0069: per-tenant lock for every authority-changing command
	personChangeGuard := command.NewPersonChangeGuard(tx, authorityLock, memberships, roles, persons, now) // last-owner guard for person-level changes
	permResolver := permissions.NewResolver(memberships, roles, time.Now)

	// Read-side adapters per ADR 00xx boundary discipline (app/query/
	// depends on the interface; concrete sqlc-aware impl lives in
	// adapters/). [audit.Reader] is declared in internal/common/audit/
	// next to its writer counterpart.
	var auditReader audit.Reader = adapters.NewAuditReaderPG(pool, tx)
	var searchIndex query.SearchIndex = adapters.NewSearchIndexPG(pool, tx)
	var statsReader query.PlatformStatsReader = adapters.NewPlatformStatsReaderPG(pool, tx)

	stampCache := adapters.NewSecurityStampCache(hybridCache, persons)
	// A committed write that rotates a stamp evicts the cached one right after the
	// commit, in this process: the old access token stops working at once instead
	// of up to the 30 s TTL (the cache is the shared L2, so every instance sees it).
	persons.WithStampEvictor(stampCache)
	// A stamp eviction that fails after the commit is counted and logged (rate-limited),
	// not swallowed: until the cache expires the old access token still works.
	persons.WithEvictionTelemetry(slog.Default(), otel.Meter("leadkart/identity"))
	// One impersonation session store serves the session commands AND the request
	// gate (ADR 0069 D2): an impersonation token is live only while its session is.
	// v0.2 ships in-memory (single-process / integration-test fit); production
	// multi-replica drops in a Redis-backed implementation behind the same
	// [impersonation.Store] interface - composition root change only.
	impersonationStore := adapters.NewImpersonationInMemoryStore(now)
	stampValidator := adapters.NewSecurityStampValidator(stampCache).WithImpersonationSessions(impersonationStore)

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

	// The refresh successor is derived, not stored: HMAC under a key derived from
	// the JWT signing secret (ADR 0068 D10). Nothing new to provision.
	successorKey, err := refreshmint.DeriveSuccessorKey([]byte(cfg.JWT.SigningKey))
	if err != nil {
		return identityWiring{}, fmt.Errorf("derive refresh successor key: %w", err)
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

	// Every command that hashes or verifies a password runs under ONE gate: the
	// process-wide bound on concurrent Argon2id work (ADR 0068 D12).
	argonCfg := cfg.Argon2
	if argonCfg.MaxConcurrent == 0 {
		argonCfg = config.Defaults().Argon2 // partial test configs; Validate guarantees it in production
	}
	argonGate := argon2.NewGate(argonCfg.MaxConcurrent, argonCfg.QueueTimeout,
		argon2.WithPerClient(argonCfg.MaxPerClient), argon2.WithReserved(argonCfg.ReservedAuthenticated))
	changePasswordLimit := cfg.RateLimit.ChangePassword
	if changePasswordLimit.Burst == 0 {
		changePasswordLimit = config.Defaults().RateLimit.ChangePassword
	}
	changePasswordAttempts := httpmw.NewKeyedLimiter(httpmw.LimiterConfig{
		RatePerSecond: changePasswordLimit.PerMinute / 60,
		Burst:         changePasswordLimit.Burst,
	})
	// The phantom counter's key derives from the server secret (the JWT signing
	// key), so no plain or unsalted-hashed address of a non-user is ever stored.
	// A dedicated key (LEADKART_IDENTITY__LOCKOUT_KEY) so that rotating the JWT signing
	// key does not reset the phantom counters while real accounts stay locked. Outside
	// production an unset key falls back to the signing key, with a WARN.
	lockoutKey, err := cfg.Identity.LockoutKeyBytes()
	if err != nil {
		return identityWiring{}, err
	}
	if lockoutKey == nil {
		slog.Warn("LEADKART_IDENTITY__LOCKOUT_KEY is not set: the phantom lockout key is derived from the JWT signing key, so a signing-key rotation resets the phantom counters while real accounts stay locked (a short 423-vs-401 oracle). Required in production.")
		lockoutKey = []byte(cfg.JWT.SigningKey)
	}
	phantomLockout := adapters.NewPhantomLockoutRepository(pool, tx, lockoutKey)

	return identityWiring{
		ArgonGate:      argonGate,
		Issuer:         issuer,
		StampCache:     stampCache,
		StampValidator: stampValidator,
		Families:       families,
		Persons:        persons,
		App: app.Application{
			Commands: app.Commands{
				RegisterTenant:       command.NewRegisterTenantHandler(tx, tenants, persons, memberships, roles, now, newTenantID, newPersonID, newMembershipID).WithArgonGate(argonGate),
				Login:                command.NewLoginHandler(authRouter, families, tenants, persons, permResolver, issuer, now, cfg.Refresh.AbsoluteTTL, cfg.Refresh.SlidingTTL, dummyHash, newFamilyID, phantomLockout, tx).WithArgonGate(argonGate),
				Refresh:              command.NewRefreshHandler(families, persons, memberships, tenants, permResolver, issuer, now, cfg.Refresh.AbsoluteTTL, cfg.Refresh.SlidingTTL, cfg.Refresh.GraceWindow, successorKey),
				Logout:               command.NewLogoutHandler(families, now),
				ChangePassword:       command.NewChangePasswordHandler(persons, families, tenants, tx, breachChecker, now).WithArgonGate(argonGate).WithAttemptLimiter(changePasswordAttempts),
				RevokeSession:        command.NewRevokeSessionHandler(families, now),
				RevokeAllSessions:    command.NewRevokeAllSessionsHandler(families, now),
				RequestPasswordReset: command.NewRequestPasswordResetHandler(persons, now),
				ConfirmPasswordReset: command.NewConfirmPasswordResetHandler(persons, families, memberships, tenants, tx, breachChecker, now).WithArgonGate(argonGate),
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

				UpdateUserProfile:              command.NewUpdateUserProfileHandler(tx, authorityLock, memberships, roles, persons, now),
				DeactivateUser:                 command.NewDeactivateUserHandler(tx, authorityLock, memberships, roles, persons, now),
				ReactivateUser:                 command.NewReactivateUserHandler(tx, authorityLock, memberships, roles, persons, now),
				AssignUserRole:                 command.NewAssignUserRoleHandler(tx, authorityLock, memberships, roles, persons, now),
				RevokeUserRole:                 command.NewRevokeUserRoleHandler(tx, authorityLock, memberships, roles, persons, now),
				ReplaceUserPermissionOverrides: command.NewReplaceUserPermissionOverridesHandler(tx, authorityLock, memberships, roles, persons, now),
				AssignUserManager:              command.NewAssignUserManagerHandler(tx, authorityLock, memberships, roles, persons, now),
				RemoveUserManager:              command.NewRemoveUserManagerHandler(tx, authorityLock, memberships, roles, persons, now),
				CreateUser:                     command.NewCreateUserHandler(tx, persons, memberships, tenants, now, newPersonID, newMembershipID).WithAuthority(authorityLock, roles).WithArgonGate(argonGate),
				AnonymiseUser:                  command.NewAnonymiseUserHandler(personChangeGuard, memberships, persons, now),

				CreateRole:             command.NewCreateRoleHandler(tx, authorityLock, memberships, roles, persons, roleHierarchyEdges, now, newRoleID, newRoleHierarchyEdgeID),
				UpdateRole:             command.NewUpdateRoleHandler(tx, authorityLock, memberships, roles, persons, now),
				DeleteRole:             command.NewDeleteRoleHandler(tx, authorityLock, memberships, roles, persons, now),
				ReplaceRolePermissions: command.NewReplaceRolePermissionsHandler(tx, authorityLock, memberships, roles, persons, now),
				GrantRolePermission:    command.NewGrantRolePermissionHandler(tx, authorityLock, memberships, roles, persons, now),
				RevokeRolePermission:   command.NewRevokeRolePermissionHandler(tx, authorityLock, memberships, roles, persons, now),
				SetRoleParent:          command.NewSetRoleParentHandler(tx, authorityLock, memberships, roles, persons, roleHierarchyEdges, now, newRoleHierarchyEdgeID), // ADR 0058

				GlobalSuspendPerson:        command.NewGlobalSuspendPersonHandler(personChangeGuard, persons, now),
				LiftPersonGlobalSuspension: command.NewLiftPersonGlobalSuspensionHandler(persons, now),
				AnonymisePerson:            command.NewAnonymisePersonHandler(personChangeGuard, persons, now),
				UpdatePersonProfile:        command.NewUpdatePersonProfileHandler(persons, now),
				HardDeleteTenant:           command.NewHardDeleteTenantHandler(tenants, memberships, tx, now),

				CreateImpersonationSession: command.NewCreateImpersonationSessionHandler(impersonationStore, tenants, issuer, now),
				EndImpersonationSession:    command.NewEndImpersonationSessionHandler(impersonationStore),

				// Permission-elevation approval workflow (ADR 0055).
				RequestPermissionElevation: command.NewRequestPermissionElevationHandler(permissionRequests, memberships, now, newPermissionRequestID),
				ApprovePermissionRequest:   command.NewApprovePermissionRequestHandler(tx, authorityLock, permissionRequests, memberships, roles, persons, now, newOverrideID),
				DenyPermissionRequest:      command.NewDenyPermissionRequestHandler(tx, authorityLock, permissionRequests, memberships, roles, persons, now),
				CancelPermissionRequest:    command.NewCancelPermissionRequestHandler(tx, authorityLock, permissionRequests, now),

				AuthorizePermissionRequestView:  command.NewAuthorizePermissionRequestViewHandler(tx, permissionRequests, memberships, roles, persons, now),
				ListDecidablePermissionRequests: command.NewListDecidablePermissionRequestsHandler(tx, permissionRequests, memberships, roles, persons, now),
			},
			Queries: app.Queries{
				ListSessions: query.NewListSessionsHandler(families),
				GetCapabilities: query.NewCachedGetCapabilitiesHandler(
					query.NewGetCapabilitiesHandler(persons, memberships, roles),
					hybridCache,
					memberships,
				),
				GetOwnPasswordPolicy:      query.NewGetOwnPasswordPolicyHandler(tenants),
				GetTenant:                 query.NewGetTenantHandler(tenants),
				GetTenantBySlug:           query.NewGetTenantBySlugHandler(tenants),
				GetUser:                   query.NewGetUserHandler(memberships, persons),
				GetUserOverrides:          query.NewGetUserOverridesHandler(memberships, roles, persons, now),
				SearchMembers:             query.NewSearchMembersHandler(adapters.NewMemberSearchPG(pool, tx)),
				ListUsers:                 query.NewListUsersHandler(memberships, persons),
				ListUsersPaged:            query.NewListUsersPagedHandler(memberships, persons),
				CountUsersByStatus:        query.NewCountUsersByStatusHandler(memberships),
				GetRole:                   query.NewGetRoleHandler(roles, roleHierarchyEdges),
				ListRoles:                 query.NewListRolesHandler(roles, roleHierarchyEdges),
				GetPerson:                 query.NewGetPersonHandler(persons),
				GetPersonByEmail:          query.NewGetPersonByEmailHandler(persons),
				ListPersonMemberships:     query.NewListPersonMembershipsHandler(memberships, persons),
				ListAllTenants:            query.NewListAllTenantsHandler(tenants),
				ListImpersonationSessions: query.NewListImpersonationSessionsHandler(impersonationStore, tenants),
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
	tiers := platformadapters.NewLeadTierReader(pool)
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
				PurchaseLead:            platformcommand.NewPurchaseLeadHandler(tx, leads, credits, tiers, outboxEnq, now, newPurchaseID),
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
func buildInventoryApp(pool *pgxpool.Pool, hybridCache *cache.HybridCache) inventoryapp.Application {
	tx := pg.NewTransactor(pool)
	products := inventoryadapters.NewProductRepository(pool, tx)
	productReader := inventoryadapters.NewProductReader(pool, tx)
	batches := inventoryadapters.NewBatchRepository(pool, tx)
	movements := inventoryadapters.NewStockMovementRepository(pool, tx)
	gstDefaults := refdata.NewGstDefaultReaderPG(pool, hybridCache)

	// Inventory aggregate-ID factories (Pure Domain refactor).
	newProductID := func() product.ID { return product.ID(ids.NewV7().String()) }
	newBatchID := func() batch.ID { return batch.ID(ids.NewV7().String()) }
	newMovementID := func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) }

	return inventoryapp.Application{
		Commands: inventoryapp.Commands{
			CreateProduct:    inventorycommand.NewCreateProductHandler(products, gstDefaults, time.Now, newProductID),
			UpdateProduct:    inventorycommand.NewUpdateProductHandler(products, time.Now),
			DeleteProduct:    inventorycommand.NewDeleteProductHandler(products, batches, time.Now),
			AddBatch:         inventorycommand.NewAddBatchHandler(tx, products, batches, movements, time.Now, newBatchID, newMovementID),
			LogStockMovement: inventorycommand.NewLogStockMovementHandler(tx, batches, movements, time.Now, newMovementID),
		},
		Queries: inventoryapp.Queries{
			GetProduct:             inventoryquery.NewGetProductHandler(productReader, time.Now),
			ListProductsPage:       inventoryquery.NewListProductsPageHandler(productReader, time.Now),
			GetBatch:               inventoryquery.NewGetBatchHandler(batches),
			ListBatchesByProduct:   inventoryquery.NewListBatchesByProductHandler(batches),
			ListBatchMovementsPage: inventoryquery.NewListBatchMovementsPageHandler(movements),
			FefoBatches:            inventoryquery.NewFefoBatchesHandler(batches, time.Now),
			ListProductCategories:  inventoryquery.NewListProductCategoriesHandler(inventoryadapters.NewProductCategoryReader(gstDefaults)),
			GetPolicy:              inventoryquery.NewGetPolicyHandler(inventoryadapters.NewPolicyReplicaRepository(pool, tx)),
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
	reminders := crmadapters.NewReminderRepository(pool, tx)

	newCrmLeadID := func() crmlead.ID { return crmlead.ID(ids.NewV7().String()) }
	newCallLogID := func() calllog.ID { return calllog.ID(ids.NewV7().String()) }
	newHistoryID := func() assignmenthistory.ID { return assignmenthistory.ID(ids.NewV7().String()) }
	newReminderID := func() crmreminder.ID { return crmreminder.ID(ids.NewV7().String()) }

	return crmWiringResult{
		App: crmapp.Application{
			Commands: crmapp.Commands{
				IngestPurchasedLead:   crmcommand.NewIngestPurchasedLeadHandler(leads, now, newCrmLeadID),
				AssignLead:            crmcommand.NewAssignLeadHandler(leads, history, tx, now, newHistoryID),
				ChangeLeadStage:       crmcommand.NewChangeLeadStageHandler(leads, now),
				ChangeLeadTemperature: crmcommand.NewChangeLeadTemperatureHandler(leads, now),
				LogCall:               crmcommand.NewLogCallHandler(leads, calls, tx, now, newCallLogID),
				ConvertLead:           crmcommand.NewConvertLeadHandler(leads, now),
				LoseLead:              crmcommand.NewLoseLeadHandler(leads, now),
				CreateReminder:        crmcommand.NewCreateReminderHandler(leads, reminders, tx, now, newReminderID),
				MarkReminderSent:      crmcommand.NewMarkReminderSentHandler(reminders, now),
				CancelReminder:        crmcommand.NewCancelReminderHandler(reminders, now),
			},
			Queries: crmapp.Queries{
				GetLead:              crmquery.NewGetLeadHandler(leads),
				ListLeads:            crmquery.NewListLeadsHandler(leads),
				ListPendingReminders: crmquery.NewListPendingRemindersHandler(reminders),
			},
		},
	}
}

// ----- Tasks wiring ----------------------------------------------------------

// buildTasksApp wires the Tasks module per BRD §6.8 (Phase C.2). Shares
// the pool + transactor with Identity / Platform / CRM. HierarchyReader
// + MembershipReader bind to the identity-side composition-root
// adapters declared in tasks_hierarchy_bridge.go.
func buildTasksApp(pool *pgxpool.Pool, now func() time.Time) tasksapp.Application {
	tx := pg.NewTransactor(pool)

	repo := tasksadapters.NewWorkItemRepository(pool, tx)
	hierarchy := newTasksHierarchyAdapter(pool, tx)
	members := newTasksMembershipAdapter(pool, tx)
	newWorkItemID := func() workitem.ID { return workitem.ID(ids.NewV7().String()) }

	return tasksapp.Application{
		Commands: tasksapp.Commands{
			CreateWorkItem:        taskscommand.NewCreateWorkItemHandler(repo, members, now, newWorkItemID),
			StartWorkItem:         taskscommand.NewStartWorkItemHandler(repo, now),
			CompleteWorkItem:      taskscommand.NewCompleteWorkItemHandler(repo, now),
			CancelWorkItem:        taskscommand.NewCancelWorkItemHandler(repo, now),
			ReassignWorkItem:      taskscommand.NewReassignWorkItemHandler(repo, hierarchy, members, now),
			MarkOverdue:           taskscommand.NewMarkOverdueHandler(repo, now),
			AutoCreateFromCallLog: taskscommand.NewAutoCreateFromCallLogHandler(repo, now, newWorkItemID),
			AutoCreateFollowUp:    taskscommand.NewAutoCreateFollowUpHandler(repo, now, newWorkItemID),
			AutoCompleteBySource:  taskscommand.NewAutoCompleteBySourceHandler(repo, now),
		},
		Queries: tasksapp.Queries{
			GetWorkItem:   tasksquery.NewGetWorkItemHandler(repo),
			ListWorkItems: tasksquery.NewListWorkItemsHandler(repo),
			Dashboard:     tasksquery.NewDashboardHandler(repo, hierarchy, now),
		},
	}
}

// buildCompanySettingsApp wires the company_settings module (ADR 0070): the
// versioned commerce policy, its history and the GST rate list. The tenant
// registered seed subscriber and the daily republish job live in cmd/worker.
func buildCompanySettingsApp(pool *pgxpool.Pool, now func() time.Time) companysettingsapp.Application {
	tx := pg.NewTransactor(pool)
	repo := companysettingsadapters.NewSettingsRepository(pool, tx)
	history := companysettingsadapters.NewSettingsHistoryReader(pool, tx)
	newRateID := func() string { return ids.NewV7().String() }

	return companysettingsapp.Application{
		Commands: companysettingsapp.Commands{
			ChangeSettings: companysettingscommand.NewChangeSettingsHandler(repo, now),
			AddGSTRate:     companysettingscommand.NewAddGSTRateHandler(repo, now, newRateID),
			UpdateGSTRate:  companysettingscommand.NewUpdateGSTRateHandler(repo, now),
			SeedSettings:   companysettingscommand.NewSeedSettingsHandler(repo, now),
			Republish:      companysettingscommand.NewRepublishHandler(repo, now),
		},
		Queries: companysettingsapp.Queries{
			GetSettings:  companysettingsquery.NewGetSettingsHandler(repo),
			ListHistory:  companysettingsquery.NewListHistoryHandler(history),
			ListGSTRates: companysettingsquery.NewListGSTRatesHandler(repo),
		},
	}
}

// buildDispatchApp wires the Dispatch module per ADR 0063 (BRD §6.6).
// Shares the pool + transactor; the create command runs under a UoW while
// the status transitions use the repository's own UpdateByID tx.
func buildDispatchApp(pool *pgxpool.Pool, now func() time.Time) dispatchapp.Application {
	tx := pg.NewTransactor(pool)
	repo := dispatchadapters.NewConsignmentNoteRepository(pool, tx)
	newID := func() consignmentnote.ID { return consignmentnote.ID(ids.NewV7().String()) }

	return dispatchapp.Application{
		Commands: dispatchapp.Commands{
			CreateConsignmentNote: dispatchcommand.NewCreateConsignmentNoteHandler(tx, repo, now, newID),
			MarkDispatched:        dispatchcommand.NewMarkDispatchedHandler(repo, now),
			MarkInTransit:         dispatchcommand.NewMarkInTransitHandler(repo, now),
			MarkDelivered:         dispatchcommand.NewMarkDeliveredHandler(repo, now),
			MarkFailed:            dispatchcommand.NewMarkFailedHandler(repo, now),
		},
		Queries: dispatchapp.Queries{
			GetConsignmentNote:        dispatchquery.NewGetConsignmentNoteHandler(repo),
			GetConsignmentNoteByOrder: dispatchquery.NewGetConsignmentNoteByOrderHandler(repo),
		},
	}
}

// buildOrdersApp wires the Orders module per ADR 0063 (BRD §6.4): the
// quote→order lifecycle, gapless FY invoice numbering, payments + credit
// notes. Saga subscribers (auto-invoice, consignment-created/delivered) live
// in cmd/worker.
func buildOrdersApp(pool *pgxpool.Pool, now func() time.Time) ordersapp.Application {
	tx := pg.NewTransactor(pool)
	quotations := ordersadapters.NewQuotationRepository(pool, tx)
	orders := ordersadapters.NewOrderRepository(pool, tx)
	invoices := ordersadapters.NewInvoiceRepository(pool, tx)
	creditNotes := ordersadapters.NewCreditNoteRepository(pool, tx)
	payments := ordersadapters.NewPaymentRepository(pool, tx)
	allocator := ordersadapters.NewInvoiceNumberAllocator(pool)
	enqueuer := ordersadapters.NewOutboxEnqueuer()
	newQuotationID := func() quotation.ID { return quotation.ID(ids.NewV7().String()) }
	newOrderID := func() order.ID { return order.ID(ids.NewV7().String()) }
	newInvoiceID := func() invoice.ID { return invoice.ID(ids.NewV7().String()) }
	newCreditNoteID := func() creditnote.ID { return creditnote.ID(ids.NewV7().String()) }
	newPaymentID := func() payment.ID { return payment.ID(ids.NewV7().String()) }

	return ordersapp.Application{
		Commands: ordersapp.Commands{
			CreateQuotation:    orderscommand.NewCreateQuotationHandler(quotations, now, newQuotationID),
			ReviseQuotation:    orderscommand.NewReviseQuotationHandler(quotations, now),
			ApproveQuotation:   orderscommand.NewApproveQuotationHandler(tx, quotations, orders, now, newOrderID),
			RejectQuotation:    orderscommand.NewRejectQuotationHandler(quotations, now),
			RecordTokenPayment: orderscommand.NewRecordTokenPaymentHandler(tx, orders, payments, now, newPaymentID),
			ConfirmOrder:       orderscommand.NewConfirmOrderHandler(tx, orders, enqueuer, now),
			PackOrder:          orderscommand.NewPackOrderHandler(tx, orders, enqueuer, now),
			InvoiceOrder:       orderscommand.NewInvoiceOrderHandler(tx, orders, invoices, allocator, now, newInvoiceID),
			AttachConsignment:  orderscommand.NewAttachConsignmentHandler(orders, now),
			MarkOrderDelivered: orderscommand.NewMarkOrderDeliveredHandler(orders, now),
			CompleteOrder:      orderscommand.NewCompleteOrderHandler(orders, now),
			CancelOrder:        orderscommand.NewCancelOrderHandler(orders, now),
			RecordPayment:      orderscommand.NewRecordPaymentHandler(orders, payments, now, newPaymentID),
			MintCreditNote:     orderscommand.NewMintCreditNoteHandler(tx, creditNotes, invoices, orders, allocator, now, newCreditNoteID),
		},
		Queries: ordersapp.Queries{
			GetOrder:            ordersquery.NewGetOrderHandler(orders),
			GetQuotation:        ordersquery.NewGetQuotationHandler(quotations),
			GetInvoiceByOrder:   ordersquery.NewGetInvoiceByOrderHandler(invoices),
			ListPaymentsByOrder: ordersquery.NewListPaymentsByOrderHandler(payments),
			GetPolicy:           ordersquery.NewGetPolicyHandler(ordersadapters.NewPolicyReplicaRepository(pool, tx)),
		},
	}
}

// buildNotificationsApp wires the Notifications module per BRD §6.9. The HTTP
// inbox (this app) is the read + self-service-action side; the cross-module
// fan-out subscribers live in cmd/worker.
func buildNotificationsApp(pool *pgxpool.Pool, now func() time.Time) notificationsapp.Application {
	tx := pg.NewTransactor(pool)
	repo := notificationsadapters.NewNotificationRepository(pool, tx)
	newID := func() notification.ID { return notification.ID(ids.NewV7().String()) }

	return notificationsapp.Application{
		Commands: notificationsapp.Commands{
			CreateNotification: notificationscommand.NewCreateNotificationHandler(repo, now, newID),
			MarkRead:           notificationscommand.NewMarkNotificationReadHandler(repo, now),
			Dismiss:            notificationscommand.NewDismissNotificationHandler(repo, now),
			MarkAllRead:        notificationscommand.NewMarkAllReadHandler(repo, now),
		},
		Queries: notificationsapp.Queries{
			ListInbox:   notificationsquery.NewListInboxHandler(repo),
			UnreadCount: notificationsquery.NewUnreadCountHandler(repo),
		},
	}
}
