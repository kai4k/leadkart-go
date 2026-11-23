package httpmw

import (
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/idempotency"
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
//	    httpmw.RequestLog(log, trusted),
//	    httpmw.Recover(log),
//	)(mux)
//
// is equivalent to:
//
//	handler := httpmw.Correlation()(httpmw.RequestLog(log, trusted)(httpmw.Recover(log)(mux)))
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

	// IdempotencyKeyer scopes X-Command-Id records per caller (and may mark a
	// request [idempotency.Ineligible]). Nil means idempotency is off: the header
	// is ignored. cmd/api passes authn.IdempotencyCallerKeyer, whose scope is the
	// verified tenant + person + membership + session + tenant scope + route.
	IdempotencyKeyer idempotency.CallerKeyer

	// LimiterNow is the clock of the limiters' sweeps and refills. Nil means
	// time.Now; tests inject a fixed or stepped clock.
	LimiterNow func() time.Time

	// TrustedProxies are the reverse proxies (the web BFF, an ingress) whose
	// X-Forwarded-For is believed; see [ClientIP]. Empty = trust nobody: every
	// client is keyed by its socket address.
	TrustedProxies []netip.Prefix

	// RouteRateLimits are the tighter per-route buckets for credential and
	// abuse-prone anonymous routes, applied after the global IP limiter. Empty =
	// none.
	RouteRateLimits RouteRateLimits
}

func (c PublicChainConfig) limiterClock() func() time.Time {
	if c.LimiterNow != nil {
		return c.LimiterNow
	}
	return time.Now
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
	ipLimiter := newRateLimiter(cfg.IPRateLimit, keyer, cfg.limiterClock())

	idemKeyer := cfg.IdempotencyKeyer
	if idemKeyer == nil {
		// No scope policy, no idempotency: X-Command-Id is ignored. A scope is a
		// security decision (who may replay whose answer), so there is no default
		// that guesses one; cmd/api passes authn.IdempotencyCallerKeyer.
		idemKeyer = func(*http.Request) string { return idempotency.Ineligible }
	}

	mws := []Middleware{
		Correlation(),
		SecurityHeaders(),
		RequestLog(cfg.Logger, cfg.TrustedProxies),
		Recover(cfg.Logger),
		BodyLimit(DefaultMaxBodyBytes),
		ClientAddress(cfg.Logger, cfg.TrustedProxies),
		ipLimiter.Middleware(),
	}
	if len(cfg.RouteRateLimits) > 0 {
		mws = append(mws, routeRateLimit(cfg.RouteRateLimits, keyer, cfg.limiterClock()))
		// A second, coarser bucket per route for IPv6 clients, keyed by /48 at
		// IPv6BlockRateMultiplier times the per-/64 rate: without it one /48 is 65 536
		// /64s, each with its own full bucket. IPv4 has no block key and is unaffected.
		mws = append(mws, routeRateLimit(scaledRouteLimits(cfg.RouteRateLimits, IPv6BlockRateMultiplier), ClientBlockKeyer(cfg.TrustedProxies), cfg.limiterClock()))
	}
	mws = append(mws, idempotency.Middleware(cfg.IdempotencyStore, cfg.Now, cfg.IdempotencyTTL, idemKeyer, cfg.Logger))
	return Chain(mws...)
}

// scaledRouteLimits returns routes with every bucket's rate and burst multiplied by
// factor.
func scaledRouteLimits(routes RouteRateLimits, factor int) RouteRateLimits {
	out := make(RouteRateLimits, len(routes))
	for route, c := range routes {
		c.RatePerSecond *= float64(factor)
		c.Burst *= factor
		out[route] = c
	}
	return out
}
