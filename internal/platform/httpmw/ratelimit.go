package httpmw

import (
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/time/rate"
)

// errCodeRateLimited is the wire-stable code for 429 responses.
const errCodeRateLimited = "rate_limited"

// LimiterKey identifies the bucket a request rate-limits against.
// Empty means "skip rate limiting" — used as the sentinel for
// requests that don't fit the keying scheme (e.g. localhost
// healthchecks where RemoteAddr can't be parsed).
type LimiterKey string

// LimiterKeyer extracts the bucket key from a request. The IP-based
// keyer ([IPLimiterKeyer]) is the v0.2 default; tenant-based keyers
// can be composed by middleware that runs after auth has bound the
// tenant ctx.
type LimiterKeyer func(r *http.Request) LimiterKey

// IPLimiterKeyer keys by the client IP, parsed out of the connection's
// RemoteAddr. Returns empty if RemoteAddr is malformed — the limiter
// treats empty as "skip" rather than degenerating to a single shared
// bucket (which would make the limiter a global circuit breaker).
//
// X-Forwarded-For trust: NOT applied here. Per OWASP "Token Theft &
// Replay", trusting client-supplied headers without an upstream proxy
// allowlist lets attackers bypass per-IP limits by spoofing. v0.3
// adds a [TrustedProxyKeyer] that pulls from XFF only when the
// immediate connection is from a known reverse proxy CIDR.
func IPLimiterKeyer(r *http.Request) LimiterKey {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Some test harnesses use bare-IP RemoteAddr; tolerate.
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return ""
	}
	return LimiterKey(host)
}

// LimiterConfig tunes a [RateLimiter].
type LimiterConfig struct {
	// RatePerSecond is the steady-state token-bucket fill rate.
	// e.g. 10.0 = 10 requests per second per key.
	RatePerSecond float64
	// Burst is the maximum tokens the bucket can hold (i.e. the
	// largest burst a key can absorb before throttling kicks in).
	Burst int
}

// RateLimiter is an in-memory per-key token bucket.
//
// v0.2 ships single-replica only — each API host runs its own
// limiter map; horizontal scaling means N replicas multiply the
// effective limit. Acceptable for the v0.2 single-replica deploy
// shape per ADR 0024 (Chainguard distroless static, single pod);
// v0.3 swaps to a Redis-backed limiter via the same surface.
//
// Eviction: limiters never expire. With UUID-shaped keys (tenant IDs,
// or a stable pool of client IPs), the map grows linearly with the
// distinct keyspace. For the IP keyer + a million distinct attackers
// per day, that's ~20 MB of *rate.Limiter instances — tolerable for
// v0.2. Eviction lands in v0.3 alongside the Redis swap.
type RateLimiter struct {
	mu       sync.Mutex
	limiters map[LimiterKey]*rate.Limiter
	r        rate.Limit
	burst    int
	keyer    LimiterKeyer
}

// NewRateLimiter constructs an in-memory limiter against the supplied
// keyer.
func NewRateLimiter(cfg LimiterConfig, keyer LimiterKeyer) *RateLimiter {
	if cfg.RatePerSecond <= 0 {
		panic("httpmw: RateLimiter requires RatePerSecond > 0")
	}
	if cfg.Burst <= 0 {
		panic("httpmw: RateLimiter requires Burst > 0")
	}
	if keyer == nil {
		panic("httpmw: RateLimiter requires keyer")
	}
	return &RateLimiter{
		limiters: make(map[LimiterKey]*rate.Limiter),
		r:        rate.Limit(cfg.RatePerSecond),
		burst:    cfg.Burst,
		keyer:    keyer,
	}
}

// limiterFor returns the per-key bucket, creating it on first use.
func (l *RateLimiter) limiterFor(key LimiterKey) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.limiters[key]; ok {
		return existing
	}
	created := rate.NewLimiter(l.r, l.burst)
	l.limiters[key] = created
	return created
}

// Middleware returns the http.Handler-shaped middleware. Empty key
// (keyer returned "") skips the limit — the request flows through.
//
// 429 response shape mirrors other LeadKart errors: JSON body with
// error code + message; no Retry-After header for v0.2 (rate-limited
// callers don't need precise scheduling at this layer; they just need
// to back off).
func (l *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := l.keyer(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !l.limiterFor(key).Allow() {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"` + errCodeRateLimited + `","message":"too many requests"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
