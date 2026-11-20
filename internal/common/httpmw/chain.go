package httpmw

import (
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

// Middleware is the canonical http.Handler-shaped middleware
// signature used throughout the codebase. Aliased here so the chain
// composer doesn't repeat the long form everywhere.
type Middleware = func(http.Handler) http.Handler

// Chain composes a list of middleware into a single function applied
// outer-first → inner-last. Equivalent to nesting the calls by hand
// but easier to read + reorder.
//
// Example:
//
//	handler := httpmw.Chain(
//	    httpmw.Correlation(),
//	    httpmw.RequestLog(log),
//	    httpmw.Recover(log),
//	)(mux)
//
// is equivalent to:
//
//	handler := httpmw.Correlation()(httpmw.RequestLog(log)(httpmw.Recover(log)(mux)))
//
// The first arg is the OUTERMOST layer; the last arg is the layer
// closest to the inner handler.
func Chain(mws ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		// Apply right-to-left so the FIRST mw in the slice ends up
		// OUTERMOST in the resulting wrapper.
		h := next
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

// PublicChainConfig configures the canonical public-API middleware
// chain. All required fields panic if zero.
type PublicChainConfig struct {
	// Logger is the destination for request logs + recover panic logs.
	// Required.
	Logger *slog.Logger

	// IdempotencyStore backs the X-Command-Id replay protection. Pass
	// idempotency.NewStore(pool) in production. Required.
	IdempotencyStore idempotency.Store

	// IdempotencyTTL is the per-record retention. Zero falls back to
	// idempotency.DefaultTTL (24h, Stripe canon).
	IdempotencyTTL time.Duration

	// Now is the clock. Pass time.Now in production; tests inject a
	// deterministic clock. Required.
	Now func() time.Time

	// IPRateLimit configures the per-IP token bucket for anonymous +
	// pre-auth traffic. Zero values panic — rate limiting is required
	// per security.md.
	IPRateLimit LimiterConfig

	// IdempotencyKeyer overrides the default scope used by the idempotency
	// middleware to key X-Command-Id records per caller. Nil → the tenant from
	// ctx (never set this early in the chain) else "anon:<client ip>" with the
	// trusted-proxy client address. cmd/api passes authn.IdempotencyCallerKeyer
	// so the scope is the verified tenant + person + route.
	IdempotencyKeyer idempotency.CallerKeyer

	// TrustedProxies are the reverse proxies (the web BFF, an ingress) whose
	// X-Forwarded-For is believed; see [ClientIP]. Empty = trust nobody: every
	// client is keyed by its socket address.
	TrustedProxies []netip.Prefix

	// RouteRateLimits are the tighter per-route buckets for credential and
	// abuse-prone anonymous routes, applied after the global IP limiter. Empty =
	// none.
	RouteRateLimits RouteRateLimits
}

// PublicChain returns the canonical middleware chain for the public
// HTTP API host. Order (outer → inner):
//
//	Correlation       — mint/echo X-Correlation-ID
//	SecurityHeaders   — OWASP Secure Headers floor on every response
//	RequestLog        — start/end structured log line
//	Recover           — catch panics → 500
//	BodyLimit         — 1 MiB request-body cap (OWASP API4)
//	IPRateLimit       — per-client-IP token bucket (trusted-proxy aware)
//	RouteRateLimit    — tighter per-route, per-client buckets (when configured)
//	Idempotency       — X-Command-Id replay protection
//	(per-route auth + handler — wired by ports.AddRoutes)
//
// SecurityHeaders sits OUTSIDE Recover so panic-derived 500s also carry
// the OWASP floor (X-Content-Type-Options, X-Frame-Options, HSTS,
// Referrer-Policy).
//
// otelhttp wraps THIS chain — see cmd/api/main.go. Putting otelhttp
// outside makes the OTel span cover the entire request lifecycle
// including rate-limit decisions; putting it inside misses 429s.
func PublicChain(cfg PublicChainConfig) Middleware {
	if cfg.Logger == nil {
		panic("httpmw: PublicChain Logger required")
	}
	if cfg.IdempotencyStore == nil {
		panic("httpmw: PublicChain IdempotencyStore required")
	}
	if cfg.Now == nil {
		panic("httpmw: PublicChain Now required")
	}
	if cfg.IPRateLimit.RatePerSecond <= 0 || cfg.IPRateLimit.Burst <= 0 {
		panic("httpmw: PublicChain IPRateLimit must set RatePerSecond and Burst")
	}

	keyer := ClientIPKeyer(cfg.TrustedProxies)
	ipLimiter := NewRateLimiter(cfg.IPRateLimit, keyer)

	idemKeyer := cfg.IdempotencyKeyer
	if idemKeyer == nil {
		idemKeyer = func(r *http.Request) string {
			if id, ok := tenancy.FromContext(r.Context()); ok && !id.IsZero() {
				return "tenant:" + id.String()
			}
			if ip := ClientIP(r, cfg.TrustedProxies); ip != "" {
				return "anon:" + ip
			}
			return ""
		}
	}

	mws := []Middleware{
		Correlation(),
		SecurityHeaders(),
		RequestLog(cfg.Logger),
		Recover(cfg.Logger),
		BodyLimit(DefaultMaxBodyBytes),
		ipLimiter.Middleware(),
	}
	if len(cfg.RouteRateLimits) > 0 {
		mws = append(mws, RouteRateLimit(cfg.RouteRateLimits, keyer))
	}
	mws = append(mws, idempotency.Middleware(cfg.IdempotencyStore, cfg.Now, cfg.IdempotencyTTL, idemKeyer))
	return Chain(mws...)
}
