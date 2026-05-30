package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
	wmw "github.com/ThreeDotsLabs/watermill/message/router/middleware"

	"github.com/leadkart/leadkart-go/internal/common/audit"
)

// SubscriberHandler is the typed handler shape registered against the
// Router. ctx carries propagated tenant + correlation context;
// messageID is the canonical envelope id used for idempotency dedup.
//
// Registered via [Router.AddSubscriber]; the canonical middleware
// stack wraps it in declared order (see [Router] godoc).
type SubscriberHandler func(ctx context.Context, messageID string, msg *message.Message) error

// Router wraps a Watermill message.Router with the LeadKart canonical
// middleware stack — see package godoc for the order. Each subscriber
// registered via [Router.AddSubscriber] gets the full stack: panic
// recovery, correlation propagation, tenant ctx bridge, dedup,
// audit, retry.
//
// One Router per process; cmd/api wires the in-process forwarder
// goroutine; cmd/worker (Step 13) hosts the subscriber goroutines
// against the same broker.
type Router struct {
	router   *message.Router
	sub      message.Subscriber
	receiver *IdempotentReceiver
	auditW   *audit.Writer
	log      *slog.Logger
	retry    RetryConfig
	poison   message.HandlerMiddleware
}

// Deps groups the dependencies the middleware stack requires —
// keeping the [NewRouter] signature short.
type Deps struct {
	Subscriber message.Subscriber
	// Publisher is used by the PoisonQueue middleware to salvage poisoned
	// messages to [DeadLetterTopic], and by the in-process DLQ consumer
	// to read them back. With a single pub/sub (gochannel, watermill-sql)
	// this is the same value as Subscriber.
	Publisher        message.Publisher
	Logger           *slog.Logger
	IdempotencyInbox *IdempotentReceiver
	AuditWriter      *audit.Writer
	// DeadLetters persists poisoned messages to common.dead_letter.
	// NewRouter wires a DLQ consumer on [DeadLetterTopic] that calls it.
	DeadLetters *DeadLetterWriter
	// CloseTimeout caps how long Run() waits on shutdown for in-
	// flight handlers to drain. Default 30s if zero.
	CloseTimeout time.Duration
	// Retry tunes the per-handler retry middleware. Zero values fall
	// back to production defaults (5 retries, 200ms→5s exponential).
	Retry RetryConfig
}

// RetryConfig tunes the retry-middleware exponential backoff.
type RetryConfig struct {
	MaxRetries      int
	InitialInterval time.Duration
	MaxInterval     time.Duration
	Multiplier      float64
}

// Production retry tunings — exposed as named constants so they're
// discoverable + greppable, not buried as magic numbers in the
// [DefaultRetry] literal.
//
// Sized for transient broker / DB blips (≤5s end-to-end with backoff:
// 200ms → 400ms → 800ms → 1.6s → 3.2s ≈ 6.2s cumulative). Per
// messaging.md "Retry policy — narrow catches only": handlers narrow
// errors themselves; this is the outer envelope.
const (
	defaultRetryMaxAttempts     = 5
	defaultRetryInitialInterval = 200 * time.Millisecond
	defaultRetryMaxInterval     = 5 * time.Second
	defaultRetryMultiplier      = 2.0
)

// DefaultRetry mirrors the production retry policy. Tests typically
// override with MaxRetries=1 + InitialInterval=10ms to keep test
// duration bounded.
var DefaultRetry = RetryConfig{
	MaxRetries:      defaultRetryMaxAttempts,
	InitialInterval: defaultRetryInitialInterval,
	MaxInterval:     defaultRetryMaxInterval,
	Multiplier:      defaultRetryMultiplier,
}

// defaultRouterCloseTimeout is the [Deps.CloseTimeout] fallback —
// caps how long [Router.Run] waits for in-flight handlers on shutdown.
const defaultRouterCloseTimeout = 30 * time.Second

// NewRouter constructs the router, applies global middleware, and wires
// the durable dead-letter consumer.
//
// Global middleware (declaration order — outermost first). These are
// context/observability setup that must surround every handler + every
// emitted message, so they sit outside the per-handler resilience stack:
//   - CorrelationIDMiddleware    propagate / generate correlation_id
//   - TraceContextMiddleware     extract W3C trace ctx → consumer span
//   - TenantContextMiddleware    metadata → ctx via tenancy.WithID
//
// Per-handler middleware (applied at [Router.AddSubscriber], outermost
// first) is the canonical resilience stack — see [Router.AddSubscriber]
// for the ordering rationale. Recoverer is INNERMOST so a panicking
// handler is converted to an error that Retry then retries; PoisonQueue
// is OUTSIDE Retry so a message is dead-lettered only after retries are
// exhausted (or immediately for a [NonRetryable] error).
func NewRouter(deps Deps) (*Router, error) {
	if deps.Subscriber == nil {
		return nil, errors.New("messaging: Subscriber required")
	}
	if deps.Publisher == nil {
		return nil, errors.New("messaging: Publisher required (PoisonQueue dead-letter)")
	}
	if deps.IdempotencyInbox == nil {
		return nil, errors.New("messaging: IdempotencyInbox required")
	}
	if deps.AuditWriter == nil {
		return nil, errors.New("messaging: AuditWriter required")
	}
	if deps.DeadLetters == nil {
		return nil, errors.New("messaging: DeadLetters required")
	}
	if deps.Logger == nil {
		return nil, errors.New("messaging: Logger required")
	}
	log := deps.Logger
	closeTimeout := deps.CloseTimeout
	if closeTimeout == 0 {
		closeTimeout = defaultRouterCloseTimeout
	}
	retry := deps.Retry
	if retry == (RetryConfig{}) {
		retry = DefaultRetry
	}

	wlog := watermill.NewSlogLogger(log)
	r, err := message.NewRouter(message.RouterConfig{CloseTimeout: closeTimeout}, wlog)
	if err != nil {
		return nil, fmt.Errorf("messaging: new router: %w", err)
	}

	// Global middleware — outermost wraps innermost. Setup only; the
	// resilience stack (Recoverer/Retry/PoisonQueue) is per-handler.
	r.AddMiddleware(
		CorrelationIDMiddleware,
		TraceContextMiddleware,
		TenantContextMiddleware,
	)

	// PoisonQueue salvages a handler error to DeadLetterTopic. Built once
	// + shared across handlers. context.Canceled (graceful shutdown) is
	// NOT poison — let it propagate so the message is redelivered next run.
	poison, err := wmw.PoisonQueueWithFilter(deps.Publisher, DeadLetterTopic, func(err error) bool {
		return !errors.Is(err, context.Canceled)
	})
	if err != nil {
		return nil, fmt.Errorf("messaging: poison queue: %w", err)
	}

	router := &Router{
		router:   r,
		sub:      deps.Subscriber,
		receiver: deps.IdempotencyInbox,
		auditW:   deps.AuditWriter,
		log:      log,
		retry:    retry,
		poison:   poison,
	}

	// Durable DLQ consumer — reads DeadLetterTopic + persists each poisoned
	// message to common.dead_letter. Registered directly on the raw
	// router with ONLY Recoverer: it must NOT carry the PoisonQueue/Retry
	// stack, or a failed DLQ write would re-poison into the same topic (a
	// loop). The persister is best-effort (acks on write failure).
	r.AddConsumerHandler(
		"messaging.dead_letter.persist",
		DeadLetterTopic,
		deps.Subscriber,
		func(msg *message.Message) error { return deps.DeadLetters.persist(msg.Context(), msg) },
	).AddMiddleware(wmw.Recoverer)

	return router, nil
}

// AddSubscriber registers handlerName as a subscriber on topic with the
// canonical resilience stack. Middleware order (outermost → innermost,
// which is the order passed to AddMiddleware):
//
//	PoisonQueue → Idempotency → Audit → Retry → Recoverer → handler
//
// Rationale (each position is load-bearing):
//   - Recoverer INNERMOST: a panicking handler becomes a RecoveredPanicError
//     that Retry sees and retries — panics are no longer fatal-once.
//   - Retry inside Audit: Audit records the final outcome once, after the
//     retry budget is spent (or immediately when Retry.ShouldRetry rejects a
//     [NonRetryable] error), not once per attempt.
//   - Idempotency inside PoisonQueue (NOT outermost): the dedup row is written
//     only when the inner chain returns nil — genuine success. A failed
//     handler propagates up to PoisonQueue, so NO inbox row is written and the
//     message stays replayable. If Idempotency were outermost it would see
//     PoisonQueue's swallowed nil and mark a poisoned message "processed",
//     silently blocking DLQ replay.
//   - PoisonQueue OUTERMOST: after retries exhaust it salvages the message to
//     the dead-letter topic and acks (swallows the error) so the broker does
//     not redeliver-and-re-poison. A duplicate returns nil from Idempotency,
//     so PoisonQueue never salvages it.
//
// A handler returns [NonRetryable](err) for a permanently-unprocessable
// message (malformed payload, schema mismatch) so it dead-letters at once.
func (r *Router) AddSubscriber(handlerName string, topic string, fn SubscriberHandler) {
	// AddConsumerHandler superseded AddNoPublisherHandler in Watermill v1.4
	// (jeremydmiller-style API rename); same behaviour, deprecation
	// removed in a future major.
	r.router.AddConsumerHandler(
		handlerName,
		topic,
		r.sub,
		watermillAdapter(fn),
	).AddMiddleware(
		r.poison,
		IdempotencyMiddleware(r.receiver, handlerName),
		AuditMiddleware(r.auditW, r.log),
		retryMiddleware(r.retry),
		wmw.Recoverer,
	)
}

// RawRouter exposes the underlying Watermill *message.Router so the cqrs
// EventProcessor ([NewEventProcessor]) can register its typed handlers on
// the SAME router that already carries the global setup middleware
// (CorrelationID / TraceContext / TenantContext). The per-handler
// resilience stack is NOT applied here — use [Router.AddCqrsHandler] to
// attach a cqrs handler with the canonical stack.
func (r *Router) RawRouter() *message.Router { return r.router }

// AddCqrsHandler registers one typed cqrs event handler on the router via
// the EventProcessor, then wraps it with the IDENTICAL per-handler
// resilience stack [AddSubscriber] uses — PoisonQueue → Idempotency →
// Audit → Retry → Recoverer (see [AddSubscriber] for the ordering
// rationale). The handler name for idempotency dedup is the cqrs handler's
// own HandlerName().
//
// This is the cqrs equivalent of [AddSubscriber]: same middleware, same
// DLQ isolation (the dead-letter consumer stays its own Recoverer-only
// handler from [NewRouter]) — only dispatch + payload decode move into the
// cqrs component + [WireAliasMarshaler].
func (r *Router) AddCqrsHandler(p *cqrs.EventProcessor, eh cqrs.EventHandler) error {
	h, err := p.AddHandler(eh)
	if err != nil {
		return fmt.Errorf("messaging: add cqrs handler %q: %w", eh.HandlerName(), err)
	}
	// Transactional inbox (ADR 0067 Phase-4): TransactionalIdempotency sits
	// INSIDE Retry so each retry attempt opens a FRESH tx — retrying inside an
	// aborted pgx tx fails ("current transaction is aborted"). Recoverer is
	// innermost so a panicking handler rolls its tx back and is retried. The
	// dedup row + the handler's DB writes commit atomically (effectively-once).
	h.AddMiddleware(
		r.poison,
		AuditMiddleware(r.auditW, r.log),
		retryMiddleware(r.retry),
		TransactionalIdempotencyMiddleware(r.receiver, eh.HandlerName()),
		wmw.Recoverer,
	)
	return nil
}

// AddCqrsHandlerAtLeastOnce registers a cqrs handler whose side effects are
// EXTERNAL and cannot be rolled back (email, cache eviction, SIEM emit). It
// uses run-then-INSERT dedup with Idempotency OUTERMOST — a duplicate
// short-circuits before Audit/Retry/DLQ, and no DB tx is held open across the
// external call. Same DLQ isolation as [AddCqrsHandler]; only the inbox
// semantics differ. The composition root picks the variant per handler.
func (r *Router) AddCqrsHandlerAtLeastOnce(p *cqrs.EventProcessor, eh cqrs.EventHandler) error {
	h, err := p.AddHandler(eh)
	if err != nil {
		return fmt.Errorf("messaging: add cqrs handler %q: %w", eh.HandlerName(), err)
	}
	h.AddMiddleware(
		r.poison,
		IdempotencyMiddleware(r.receiver, eh.HandlerName()),
		AuditMiddleware(r.auditW, r.log),
		retryMiddleware(r.retry),
		wmw.Recoverer,
	)
	return nil
}

// Running returns true once the router's Run loop is up — useful in
// tests + startup ordering.
func (r *Router) Running() <-chan struct{} { return r.router.Running() }

// Run starts the router's Run loop. Blocks until ctx is cancelled.
//
// Per Watermill canon: Run owns lifecycle; closing it cleanly drains
// in-flight handlers within CloseTimeout. Caller (cmd/worker or
// in-process goroutine inside cmd/api) is responsible for ctx.
func (r *Router) Run(ctx context.Context) error {
	if err := r.router.Run(ctx); err != nil {
		return fmt.Errorf("messaging: router run: %w", err)
	}
	return nil
}

// Close releases the underlying router. Idempotent.
func (r *Router) Close() error { return r.router.Close() }

// watermillAdapter translates LeadKart's [SubscriberHandler] into the
// Watermill HandlerFunc shape (returns []*message.Message — always
// nil for NoPublisher handlers; LeadKart subscribers don't return
// new messages from the handler body, they invoke services that
// publish via the outbox).
func watermillAdapter(fn SubscriberHandler) message.NoPublishHandlerFunc {
	return func(msg *message.Message) error {
		return fn(msg.Context(), msg.UUID, msg)
	}
}

// retryMiddleware applies an exponential-backoff retry policy for
// transient handler failures. A handler marks a permanently-unprocessable
// error with [NonRetryable]; ShouldRetry then skips retries so it reaches
// the PoisonQueue (DLQ) at once instead of burning the backoff budget.
func retryMiddleware(cfg RetryConfig) message.HandlerMiddleware {
	r := wmw.Retry{
		MaxRetries:      cfg.MaxRetries,
		InitialInterval: cfg.InitialInterval,
		MaxInterval:     cfg.MaxInterval,
		Multiplier:      cfg.Multiplier,
		ShouldRetry: func(p wmw.RetryParams) bool {
			return !IsNonRetryable(p.Err)
		},
	}
	return r.Middleware
}
