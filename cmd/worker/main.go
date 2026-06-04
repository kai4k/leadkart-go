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
	"github.com/ThreeDotsLabs/watermill/components/forwarder"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"golang.org/x/sync/errgroup"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/jobs"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/obs"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	crmadapters "github.com/leadkart/leadkart-go/internal/crm/adapters"
	crmcommand "github.com/leadkart/leadkart-go/internal/crm/app/command"
	crmjobs "github.com/leadkart/leadkart-go/internal/crm/app/jobs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	crmreminder "github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	crmsubscribers "github.com/leadkart/leadkart-go/internal/crm/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventoryjobs "github.com/leadkart/leadkart-go/internal/inventory/app/jobs"
	platformadapters "github.com/leadkart/leadkart-go/internal/platform/adapters"
	platformcommand "github.com/leadkart/leadkart-go/internal/platform/app/command"
	platformsubscribers "github.com/leadkart/leadkart-go/internal/platform/ports/subscribers"
	tasksadapters "github.com/leadkart/leadkart-go/internal/tasks/adapters"
	taskscommand "github.com/leadkart/leadkart-go/internal/tasks/app/command"
	tasksjobs "github.com/leadkart/leadkart-go/internal/tasks/app/jobs"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
	taskssubscribers "github.com/leadkart/leadkart-go/internal/tasks/ports/subscribers"
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
//
//nolint:cyclop // composition root — wiring depth scales with subscriber + worker count by design; refactoring into helpers obscures the single-place dependency graph the manual-wiring canon protects.
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
	// Single Watermill Forwarder drains the shared common.outbox relay and
	// republishes each event to its destination module topic (embedded in
	// the envelope by messaging.PublishOutbox). Replaces the four hand-rolled
	// per-module poll loops — per ADR 0064 the outbox is one shared relay.
	outboxForwarder, err := messaging.NewOutboxForwarder(pool, pubsub, watermill.NewSlogLogger(logger))
	if err != nil {
		return fmt.Errorf("outbox forwarder: %w", err)
	}

	crmLeads := crmadapters.NewCrmLeadRepository(pool, tx)
	crmReminders := crmadapters.NewReminderRepository(pool, tx)
	crmMatureScanner := crmadapters.NewMatureLeadScannerPG(pool, tx)
	newCrmLeadID := func() crmlead.ID { return crmlead.ID(ids.NewV7().String()) }
	newCrmReminderID := func() crmreminder.ID { return crmreminder.ID(ids.NewV7().String()) }
	crmCreateReminder := crmcommand.NewCreateReminderHandler(crmLeads, crmReminders, time.Now, newCrmReminderID)
	crmIngest := crmsubscribers.NewPurchasedLeadIngestor(
		crmcommand.NewIngestPurchasedLeadHandler(crmLeads, time.Now, newCrmLeadID), logger)
	crmCallback := crmsubscribers.NewCallbackReminderCreator(crmCreateReminder, logger)

	// Platform subscriber: identity.tenant_registered.v1 → init a zero-balance
	// LeadCredit row for the new tenant (BRD §6.2). Cross-module consumer of
	// the Identity `identity.events` topic; idempotent via natural-key precheck.
	platformCredits := platformadapters.NewLeadCreditRepository(pool, tx)
	platformTenantRegistered := platformsubscribers.NewTenantRegisteredIngestor(
		platformcommand.NewInitialiseLeadCreditsHandler(tx, platformCredits, time.Now), logger)

	// Tasks module (Phase C.2 — BRD §6.8). Writes ride the shared
	// common.outbox; the single Watermill Forwarder relays them. Tasks
	// consumes CRM's call_logged + lead_converted events for follow-up
	// auto-creation and source auto-completion.
	tasksRepo := tasksadapters.NewWorkItemRepository(pool, tx)
	newWorkItemID := func() workitem.ID { return workitem.ID(ids.NewV7().String()) }
	tasksAutoCreateFromCallLog := taskscommand.NewAutoCreateFromCallLogHandler(tasksRepo, time.Now, newWorkItemID)
	tasksAutoCreateFollowUp := taskscommand.NewAutoCreateFollowUpHandler(tasksRepo, time.Now, newWorkItemID)
	tasksAutoComplete := taskscommand.NewAutoCompleteBySourceHandler(tasksRepo, time.Now)
	tasksMarkOverdue := taskscommand.NewMarkOverdueHandler(tasksRepo, time.Now)
	tasksCallLoggedSub := taskssubscribers.NewCallLoggedSubscriber(tasksAutoCreateFromCallLog, tasksAutoComplete, logger)
	tasksLeadConvertedSub := taskssubscribers.NewLeadConvertedSubscriber(tasksAutoCreateFollowUp, logger, time.Now)

	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Publisher:        pubsub,
		Logger:           logger,
		IdempotencyInbox: messaging.NewIdempotentReceiver(pool),
		AuditWriter:      audit.NewWriter(pool, logger, time.Now),
		DeadLetters:      messaging.NewDeadLetterWriter(pool, logger, time.Now),
		CloseTimeout:     routerCloseTimeout,
		Retry:            messaging.DefaultRetry,
	})
	if err != nil {
		return fmt.Errorf("messaging router: %w", err)
	}
	// cqrs EventProcessor (ADR 0067): typed dispatch over the shared
	// router. The processor derives each handler's subscribe topic from
	// the event alias (identity.* → identity.events, platform.* →
	// platform.events) and decodes the payload via the WireAliasMarshaler;
	// router.AddCqrsHandler attaches the canonical resilience stack
	// (PoisonQueue + Idempotency + Audit + Retry + Recoverer) per handler.
	eventProcessor, err := messaging.NewEventProcessor(router.RawRouter(), pubsub, watermill.NewSlogLogger(logger))
	if err != nil {
		return fmt.Errorf("messaging event processor: %w", err)
	}

	// Gather every module's typed handlers. buildEmailSender panics on a
	// malformed no-reply address — string literal, init-time only, so
	// fail-fast at boot is the right shape (CLAUDE.md "MustNewX init-time
	// only"). CRM consumes the Platform lead-purchased event + its own
	// call-logged event (callback reminders); Platform consumes the
	// Identity tenant-registered event.
	cqrsHandlers := subscribers.Handlers(subWiring.Families, subWiring.StampCache, buildEmailSender(logger), logger, time.Now)
	cqrsHandlers = append(cqrsHandlers, crmsubscribers.Handlers(crmIngest, crmCallback)...)
	cqrsHandlers = append(cqrsHandlers, platformsubscribers.Handlers(platformTenantRegistered)...)
	cqrsHandlers = append(cqrsHandlers, taskssubscribers.Handlers(tasksCallLoggedSub, tasksLeadConvertedSub)...)
	for _, h := range cqrsHandlers {
		if err := router.AddCqrsHandler(eventProcessor, h); err != nil {
			return fmt.Errorf("register cqrs handler: %w", err)
		}
	}

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
	// CRM mature-lead daily scan (BRD §4.7) — flags converted leads
	// with no reorder activity in the configured window.
	if err := river.AddWorkerSafely(workers, crmjobs.NewMatureLeadScanWorker(
		crmMatureScanner, crmCreateReminder, logger, time.Now,
	)); err != nil {
		return fmt.Errorf("register crm mature-lead scan worker: %w", err)
	}
	// Inventory expiry + reorder daily scans (BRD §6.5) — emit alert events
	// for batches nearing expiry / products below reorder level.
	inventoryAlertScan := inventoryadapters.NewAlertScanRepository(pool)
	if err := river.AddWorkerSafely(workers, inventoryjobs.NewExpiryScanWorker(inventoryAlertScan, logger, time.Now)); err != nil {
		return fmt.Errorf("register inventory expiry scan worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, inventoryjobs.NewReorderScanWorker(inventoryAlertScan, logger, time.Now)); err != nil {
		return fmt.Errorf("register inventory reorder scan worker: %w", err)
	}
	// Tasks module river jobs (Phase C.2 — BRD §6.8): overdue scan + purge.
	if err := river.AddWorkerSafely(workers, tasksjobs.NewOverdueScanWorker(tasksRepo, tasksMarkOverdue, logger, time.Now)); err != nil {
		return fmt.Errorf("register tasks overdue scan worker: %w", err)
	}
	if err := river.AddWorkerSafely(workers, tasksjobs.NewPurgeWorker(tasksRepo, logger, time.Now)); err != nil {
		return fmt.Errorf("register tasks purge worker: %w", err)
	}
	periodics := []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(audit.PurgeInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return audit.PurgeJob{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(crmjobs.MatureLeadScanInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return crmjobs.MatureLeadScanJob{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(inventoryjobs.ExpiryScanInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return inventoryjobs.ExpiryScanJob{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(inventoryjobs.ReorderScanInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return inventoryjobs.ReorderScanJob{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(tasksjobs.OverdueScanInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return tasksjobs.OverdueScanJob{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
		river.NewPeriodicJob(
			river.PeriodicInterval(tasksjobs.PurgeInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return tasksjobs.PurgeJob{}, nil
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

	return runWorkerServices(ctx, logger, outboxForwarder, router, riverClient, adminSrv)
}

// runWorkerServices runs the worker's long-lived services (outbox forwarder,
// subscriber router, river client, admin listener) under one errgroup until
// ctx cancels, then drives graceful shutdown. Split out of run so the
// composition root stays under the cyclomatic-complexity gate.
func runWorkerServices(
	ctx context.Context,
	logger *slog.Logger,
	outboxForwarder *forwarder.Forwarder,
	router *messaging.Router,
	riverClient *jobs.Client,
	adminSrv *http.Server,
) error {
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("outbox forwarder starting")
		if err := outboxForwarder.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("outbox forwarder: %w", err)
		}
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
		logger.Info("worker admin listening", "addr", adminSrv.Addr)
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
		_ = outboxForwarder.Close()
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
