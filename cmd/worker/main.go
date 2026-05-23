// Package main is the LeadKart worker binary entrypoint.
//
// The worker owns the EVENT-PROCESSING side of the modular monolith:
// it polls every module's outbox table, publishes the rows onto a
// pub/sub layer (in-process Watermill GoChannel for v0.2; Redis Streams
// or Kafka for v0.3+), and runs the per-module subscriber router.
// `cmd/api` writes integration events to the outbox; `cmd/worker`
// drains them.
//
// Why a separate binary: per messaging.md doctrine + ADR 0008,
// subscriber lifecycles are independent of the request path. Co-
// hosting them with the public API meant a busy API host shared CPU
// with subscriber retries + exponential backoffs; one slow handler
// pinned a request thread that should have been answering customer
// traffic. Splitting also lets the worker scale horizontally on its
// own (N replicas of cmd/worker against the same Postgres outbox)
// without provisioning extra API capacity.
//
// Required environment (subset of internal/common/config/AppConfig):
//
//	LEADKART_POSTGRES__DSN              postgres DSN (leadkart_app role)
//	LEADKART_REDIS__ADDR                redis "host:port" (HybridCache L2)
//	LEADKART_LISTEN__WORKER_ADMIN       worker admin/probes listener (default ":9091")
//	LEADKART_CONFIG_FILE                optional YAML overlay before env
//
// JWT + Refresh config keys are unused by the worker (it doesn't issue
// tokens) but the validator still requires them — for v0.2 ops set
// the same values cmd/api uses; v0.3 splits the config struct.
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

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"golang.org/x/sync/errgroup"

	"github.com/riverqueue/river"

	crmadapters "github.com/leadkart/leadkart-go/internal/crm/adapters"
	crmcommand "github.com/leadkart/leadkart-go/internal/crm/app/command"
	crmintegrationevents "github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	crmsubscribers "github.com/leadkart/leadkart-go/internal/crm/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventoryintegrationevents "github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/jobs"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/obs"
	"github.com/leadkart/leadkart-go/internal/common/pg"

	platformadapters "github.com/leadkart/leadkart-go/internal/platform/adapters"
	platformintegrationevents "github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// Tunings — same shape as cmd/api/main.go so the two binaries' admin
// behaviour stays consistent. Per-binary doc strings explain why.
const (
	// healthCheckTimeout — per-checker budget on /ready probes.
	healthCheckTimeout = 2 * time.Second
	// otelShutdownTimeout — OpenTelemetry exporter flush + close.
	otelShutdownTimeout = 10 * time.Second
	// redisPingTimeout caps the boot-time Redis reachability check.
	redisPingTimeout = 5 * time.Second
	// hybridCacheL1MaxItems sizes ristretto's MaxCost (entry budget).
	hybridCacheL1MaxItems = 10_000
	// shutdownTimeout caps how long the worker waits for in-flight
	// subscriber handlers to drain on SIGTERM.
	shutdownTimeout = 30 * time.Second
	// routerCloseTimeout is the messaging.Router's per-shutdown
	// CloseTimeout — must be ≤ shutdownTimeout.
	routerCloseTimeout = 25 * time.Second
	// forwarderPollInterval — how often the forwarder polls the outbox
	// for unforwarded rows when the previous poll returned nothing.
	forwarderPollInterval = time.Second
	// forwarderRetryInterval — backoff after a publish failure before
	// retrying the same row.
	forwarderRetryInterval = 50 * time.Millisecond
	// healthcheckTimeout caps the distroless self-probe HTTP call.
	healthcheckTimeout = 3 * time.Second
	// defaultEmailLinkBaseURL is the base URL the email subscriber
	// uses for reset / confirmation links. v0.2 default pending real
	// frontend deploy. v0.3 wires LEADKART_EMAIL__APP_URL via config.
	defaultEmailLinkBaseURL = "https://app.leadkart.example"
)

func main() {
	// Distroless container HEALTHCHECK self-probe — same pattern as
	// cmd/api/main.go. The binary itself becomes the probe per
	// chainguard's "single-binary healthcheck" canon. Hits the worker
	// admin listener's /alive endpoint.
	if len(os.Args) >= 2 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		if err := healthcheck(); err != nil {
			fmt.Fprintf(os.Stderr, "leadkart-worker healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "leadkart-worker: %v\n", err)
		os.Exit(1)
	}
}

// healthcheck probes the worker admin listener's /alive endpoint.
// Reads LEADKART_LISTEN__WORKER_ADMIN to discover where the listener
// is bound — same env var the runtime config loader consumes, so probe
// + listener can never disagree.
func healthcheck() error {
	addr := os.Getenv("LEADKART_LISTEN__WORKER_ADMIN")
	if addr == "" {
		addr = ":9091"
	}
	if !strings.HasPrefix(addr, ":") {
		addr = ":" + addr
	}
	url := "http://127.0.0.1" + addr + "/alive"

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()

	// Loopback-only host (literal "127.0.0.1"); only the port is env-
	// derived. SSRF impossible for the same reasons documented on
	// cmd/api/main.go's healthcheck — see that file for the full
	// gosec annotation rationale.
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

// run is the testable entrypoint per Mat Ryer 2024.
func run(ctx context.Context, stdout *os.File) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// OTel install BEFORE the slog logger — see cmd/api/main.go for
	// the rationale (otelslog binds the LoggerProvider at handler
	// construction, not at Handle time).
	otelShutdown, err := obs.Setup(ctx, cfg.OTel)
	if err != nil {
		return fmt.Errorf("obs: %w", err)
	}

	// Worker uses a distinct service name so OTel backends can split
	// API vs. worker telemetry. cfg.OTel.ServiceName is "leadkart-api"
	// by default; override locally for the worker process.
	const workerServiceName = "leadkart-worker"
	logger := slog.New(obs.NewSlogHandler(
		slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		workerServiceName,
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
		IncludeQueryParameters: false, // PII guard — see cmd/api/main.go.
	})
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgxpool ping: %w", err)
	}
	logger.InfoContext(ctx, "postgres connected")

	// See cmd/api/main.go for the MaintNotificationsConfig rationale —
	// opt-out of the Redis Enterprise / ElastiCache CLIENT MAINT_NOTIFICATIONS
	// protocol we do not deploy against (go-redis 9.19's "auto" default
	// otherwise spawns a CircuitBreakerManager cleanup goroutine).
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

	subWiring := buildSubscriberWiring(pool, hybridCache)

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(logger))
	defer func() { _ = pubsub.Close() }()

	tx := pg.NewTransactor(pool)
	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, integrationevents.Topic, 0, time.Now)

	// Per-module outbox forwarder: each bounded context owns its own
	// outbox table (CLAUDE.md §"Each module owns its Postgres schema"),
	// so each needs its own forwarder bound to the schema-specific sqlc
	// Queries.
	inventoryForwarder := inventoryadapters.NewOutboxForwarder(pool, tx, pubsub, inventoryintegrationevents.Topic, 0, time.Now)

	// Platform-module outbox forwarder (ADR 0059). Sibling of the
	// identity forwarder — own table, own topic, own goroutine. Slice 1
	// has no in-process subscriber for platform events; the topic is
	// still drained so audit-log shape stays consistent.
	platformForwarder := platformadapters.NewOutboxForwarder(pool, tx, pubsub, platformintegrationevents.Topic, 0, time.Now)

	// CRM-module outbox forwarder (ADR 0060). Drains crm.outbox to the
	// crm.events Watermill topic. Slice 1 CRM also subscribes to the
	// platform.events topic for the lead-purchased ingest.
	crmForwarder := crmadapters.NewOutboxForwarder(pool, tx, pubsub, crmintegrationevents.Topic, 0, time.Now)
	crmLeads := crmadapters.NewCrmLeadRepository(pool, tx)
	crmIngest := crmsubscribers.NewPurchasedLeadIngestor(
		crmcommand.NewIngestPurchasedLeadHandler(crmLeads), logger)

	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Logger:           logger,
		IdempotencyInbox: messaging.NewIdempotentReceiver(pool),
		AuditWriter:      audit.NewWriter(pool, logger, time.Now),
		CloseTimeout:     routerCloseTimeout,
		Retry:            messaging.DefaultRetry,
	})
	if err != nil {
		return fmt.Errorf("messaging router: %w", err)
	}
	// Email-dispatch subscriber (ADR 0057). buildEmailSender panics on
	// malformed no-reply address — string literal, init-time only, so
	// fail-fast at boot is the right shape per CLAUDE.md "MustNewX
	// init-time only".
	subscribers.Register(router, subWiring.Families, subWiring.StampCache, buildEmailSender(logger), logger, time.Now)

	// CRM module subscribers (ADR 0060). The lead-purchased subscriber
	// rides the Platform module's `platform.events` topic — handler-side
	// event_type filtering routes only `platform.lead-purchased.v1` to
	// the ingest handler.
	crmsubscribers.Register(router, crmIngest, "platform.events", logger)

	// River background-job pool. v0.2 ships one job — AuditLogPurgeJob —
	// running daily to enforce the 7-year audit retention. River's
	// migrations run in-process at boot for the v0.2 single-replica
	// shape; v0.3 splits this into a dedicated cmd/migrate invocation.
	if err := jobs.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, audit.NewPurgeWorker(pool, logger, time.Now)); err != nil {
		return fmt.Errorf("register audit purge worker: %w", err)
	}
	periodics := []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(audit.PurgeInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return audit.PurgeJob{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
	riverClient, err := jobs.NewClient(jobs.Config{
		Pool:         pool,
		Workers:      workers,
		PeriodicJobs: periodics,
		Logger:       logger,
	})
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}

	health := obs.NewHealth([]obs.HealthChecker{
		obs.HealthCheckerFunc{N: "postgres", Fn: pool.Ping},
		obs.HealthCheckerFunc{N: "redis", Fn: func(ctx context.Context) error {
			return redisCli.Ping(ctx).Err()
		}},
	}, healthCheckTimeout)
	adminSrv := obs.NewAdminServer(cfg.Listen.WorkerAdmin, health)

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		forwarder.Run(gctx, forwarderPollInterval, forwarderRetryInterval, func(err error) {
			logger.ErrorContext(gctx, "outbox forwarder", "err", err)
		})
		return nil
	})

	g.Go(func() error {
		platformForwarder.Run(gctx, forwarderPollInterval, forwarderRetryInterval, func(err error) {
			logger.ErrorContext(gctx, "platform outbox forwarder", "err", err)
		})
		return nil
	})

	g.Go(func() error {
		inventoryForwarder.Run(gctx, forwarderPollInterval, forwarderRetryInterval, func(err error) {
			logger.ErrorContext(gctx, "inventory outbox forwarder", "err", err)
		})
		return nil
	})

	g.Go(func() error {
		crmForwarder.Run(gctx, forwarderPollInterval, forwarderRetryInterval, func(err error) {
			logger.ErrorContext(gctx, "crm outbox forwarder", "err", err)
		})
		return nil
	})

	g.Go(func() error {
		logger.Info("subscriber router starting")
		if err := router.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("router: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		logger.Info("river client starting")
		if err := riverClient.Start(gctx); err != nil {
			return fmt.Errorf("river start: %w", err)
		}
		// Block until ctx cancels; Start spawns its own background
		// goroutines so we just need to wait for the shutdown signal
		// + then drive Stop on the returned client below.
		<-gctx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := riverClient.Stop(stopCtx); err != nil {
			return fmt.Errorf("river stop: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		logger.Info("worker admin listening", "addr", cfg.Listen.WorkerAdmin)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("admin: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		<-gctx.Done()
		logger.Info("worker shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = adminSrv.Shutdown(shutdownCtx)
		return router.Close()
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// subscriberWiring groups the repos + cache facade the cmd/worker
// subscribers need. Constructed inside the worker process — distinct
// from cmd/api's identityWiring which carries Issuer + StampValidator
// (request-path concerns the worker doesn't have).
type subscriberWiring struct {
	Persons    *adapters.PersonRepository
	Families   *adapters.RefreshTokenFamilyRepository
	StampCache *adapters.SecurityStampCache
}

// buildEmailSender wires the ADR 0057 email-dispatch subscriber. v0.2
// stays Recorder-backed in dev/integration — production (v0.3+) swaps
// in a real provider via the same email.Gateway interface (composition-
// root change only). The from-address + frontend URL live HERE, not in
// cmd/api, because the SUBSCRIBER (not the command handler) is the
// boundary that actually builds the outbound message.
//
// Panics on malformed no-reply literal — init-time only, per CLAUDE.md
// "MustNewX init-time + tests only" canon. A malformed compile-time
// literal is a programmer error, not a runtime condition.
func buildEmailSender(logger *slog.Logger) *subscribers.EmailSender {
	noReplyAddress, err := email.New("no-reply@leadkart.local")
	if err != nil {
		panic(fmt.Sprintf("worker: no-reply email address: %v", err))
	}
	gateway := email.NewRecorder(time.Now)
	return subscribers.NewEmailSender(gateway, noReplyAddress, defaultEmailLinkBaseURL, logger)
}

// buildSubscriberWiring constructs the minimal wiring set the
// subscriber-side of Identity needs. Tests can call this directly with
// a testcontainers pool + miniredis-backed HybridCache.
func buildSubscriberWiring(pool *pgxpool.Pool, hybridCache *cache.HybridCache) subscriberWiring {
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	stampCache := adapters.NewSecurityStampCache(hybridCache, persons)
	return subscriberWiring{
		Persons:    persons,
		Families:   families,
		StampCache: stampCache,
	}
}
