package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill"
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
}

// Deps groups the dependencies the middleware stack requires —
// keeping the [NewRouter] signature short.
type Deps struct {
	Subscriber       message.Subscriber
	Logger           *slog.Logger
	IdempotencyInbox *IdempotentReceiver
	AuditWriter      *audit.Writer
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

// NewRouter constructs the router + applies global middleware.
//
// Global middleware (in declaration order — outermost first):
//   - watermill.Recoverer        panic → error
//   - TraceContextMiddleware     extract W3C trace ctx → consumer span
//   - CorrelationIDMiddleware    propagate / generate correlation_id
//   - TenantContextMiddleware    metadata → ctx via tenancy.WithID
//
// Trace middleware sits second-outermost (just inside Recoverer) so the
// per-message span surrounds every other middleware + the handler body
// — panic recovery still needs to be outermost so a recovered panic
// doesn't escape the trace boundary.
//
// Per-handler middleware (Idempotency + Audit + Retry) is applied at
// [Router.AddSubscriber] time so each handler's name participates in
// the dedup key. Retry sits innermost so transient handler failures
// retry under the same dedup row.
func NewRouter(deps Deps) (*Router, error) {
	if deps.Subscriber == nil {
		return nil, errors.New("messaging: Subscriber required")
	}
	if deps.IdempotencyInbox == nil {
		return nil, errors.New("messaging: IdempotencyInbox required")
	}
	if deps.AuditWriter == nil {
		return nil, errors.New("messaging: AuditWriter required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
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

	// Global middleware — outermost wraps innermost.
	r.AddMiddleware(
		wmw.Recoverer,
		TraceContextMiddleware,
		CorrelationIDMiddleware,
		TenantContextMiddleware,
	)

	return &Router{
		router:   r,
		sub:      deps.Subscriber,
		receiver: deps.IdempotencyInbox,
		auditW:   deps.AuditWriter,
		log:      log,
		retry:    retry,
	}, nil
}

// AddSubscriber registers handlerName as a subscriber on topic. The
// handler receives ctx with tenant + correlation propagated; the
// per-handler middleware stack wraps it in:
//
//	IdempotencyMiddleware(handlerName) → AuditMiddleware → Retry → handler
//
// retry caps + backoff are tuned for transient errors; non-transient
// errors should NOT retry (return them unwrapped, broker DLQs).
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
		IdempotencyMiddleware(r.receiver, handlerName),
		AuditMiddleware(r.auditW, r.log),
		retryMiddleware(r.retry),
	)
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

// retryMiddleware applies an exponential-backoff retry policy per
// messaging.md "Retry policy — narrow catches only".
//
// Non-retryable errors should bypass via the return value — Watermill
// retries everything by default; bespoke per-handler error narrowing
// is left to the handler itself (e.g. errors.Is gates).
func retryMiddleware(cfg RetryConfig) message.HandlerMiddleware {
	r := wmw.Retry{
		MaxRetries:      cfg.MaxRetries,
		InitialInterval: cfg.InitialInterval,
		MaxInterval:     cfg.MaxInterval,
		Multiplier:      cfg.Multiplier,
	}
	return r.Middleware
}
