package httpmw

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// errCodeRateLimited is the wire-stable code for 429 responses.
const errCodeRateLimited = "rate_limited"

// LimiterKey identifies the bucket a request rate-limits against.
// Empty means "skip rate limiting" — used as the sentinel for
// requests that don't fit the keying scheme (e.g. localhost
// healthchecks where RemoteAddr can't be parsed).
type LimiterKey string

// LimiterKeyer extracts the bucket key from a request. The client
// keyer is [ClientIPKeyer]; tenant-based keyers can be composed by middleware
// that runs after auth has bound the tenant ctx.
type LimiterKeyer func(r *http.Request) LimiterKey

// LimiterConfig tunes a [RateLimiter].
type LimiterConfig struct {
	// RatePerSecond is the steady-state token-bucket fill rate.
	// e.g. 10.0 = 10 requests per second per key.
	RatePerSecond float64
	// Burst is the maximum tokens the bucket can hold (i.e. the
	// largest burst a key can absorb before throttling kicks in).
	Burst int
	// MaxKeys caps the number of distinct buckets held at once. Zero means
	// [DefaultMaxLimiterKeys]. At the cap a request for a NEW key is answered
	// 429 (fail closed) after an immediate sweep of idle buckets has failed to
	// make room; keys already tracked keep their buckets.
	MaxKeys int
}

const (
	// DefaultMaxLimiterKeys bounds one limiter's map: ~200 bytes a key, so ~10 MB
	// per limiter and ~70 MB for the global limiter plus the six route limiters,
	// whatever the number of distinct sources.
	DefaultMaxLimiterKeys = 50_000

	// limiterSweepEvery spaces the idle sweeps: at most one scan a minute, run
	// inline by the request that finds one due (no goroutine per key, none at all).
	limiterSweepEvery = time.Minute
)

// RateLimiter is an in-memory per-key token bucket.
//
// v0.2 ships single-replica only: each API host runs its own limiter map; N
// replicas multiply the effective limit (ADR 0024).
//
// Memory is bounded two ways. A bucket whose tokens are back at the burst is
// indistinguishable from a fresh one, so an idle sweep (at most once a minute)
// deletes it and a later request re-creates it full. And the map has a hard cap
// ([LimiterConfig.MaxKeys]); at the cap a new key is refused with 429 rather
// than admitted unmetered. Fail-closed is the right side of that trade: reaching
// the cap needs tens of thousands of distinct sources inside the sweep interval,
// which is an attack, and refusing new sources during it costs far less than
// letting the process grow until the container is killed.
type RateLimiter struct {
	mu        sync.Mutex
	limiters  map[LimiterKey]*rate.Limiter
	r         rate.Limit
	burst     int
	maxKeys   int
	keyer     LimiterKeyer
	now       func() time.Time
	lastSweep time.Time
}

// NewRateLimiter constructs an in-memory limiter against the supplied
// keyer.
func NewRateLimiter(cfg LimiterConfig, keyer LimiterKeyer) *RateLimiter {
	return newRateLimiter(cfg, keyer, time.Now)
}

func newRateLimiter(cfg LimiterConfig, keyer LimiterKeyer, now func() time.Time) *RateLimiter {
	if cfg.RatePerSecond <= 0 {
		panic("httpmw: RateLimiter requires RatePerSecond > 0")
	}
	if cfg.Burst <= 0 {
		panic("httpmw: RateLimiter requires Burst > 0")
	}
	if keyer == nil {
		panic("httpmw: RateLimiter requires keyer")
	}
	maxKeys := cfg.MaxKeys
	if maxKeys <= 0 {
		maxKeys = DefaultMaxLimiterKeys
	}
	return &RateLimiter{
		limiters: make(map[LimiterKey]*rate.Limiter),
		r:        rate.Limit(cfg.RatePerSecond),
		burst:    cfg.Burst,
		maxKeys:  maxKeys,
		keyer:    keyer,
		now:      now,
	}
}

// sweepLocked deletes every bucket that has refilled to its burst. Caller holds
// the mutex; runs at most once per [limiterSweepEvery] unless force is set.
func (l *RateLimiter) sweepLocked(now time.Time, force bool) {
	if !force && !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < limiterSweepEvery {
		return
	}
	l.lastSweep = now
	for k, lim := range l.limiters {
		if lim.TokensAt(now) >= float64(l.burst) {
			delete(l.limiters, k)
		}
	}
}

// limiterFor returns the per-key bucket, creating it on first use. ok is false
// when the key is new and the map is full even after a sweep.
func (l *RateLimiter) limiterFor(key LimiterKey, now time.Time) (*rate.Limiter, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now, false)
	if existing, ok := l.limiters[key]; ok {
		return existing, true
	}
	if len(l.limiters) >= l.maxKeys {
		l.sweepLocked(now, true)
		if len(l.limiters) >= l.maxKeys {
			return nil, false
		}
	}
	created := rate.NewLimiter(l.r, l.burst)
	l.limiters[key] = created
	return created, true
}

// retryAfterSeconds rounds a wait UP to whole seconds (minimum 1): a client that
// waits the advertised time must not be refused again for the sub-second
// remainder.
func retryAfterSeconds(wait time.Duration) int {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

func writeRateLimited(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"` + errCodeRateLimited + `","message":"too many requests"}`))
}

// Middleware returns the http.Handler-shaped middleware. Empty key
// (keyer returned "") skips the limit — the request flows through.
//
// The 429 carries a JSON body and a `Retry-After` header (RFC 6585 §4,
// RFC 9110 §10.2.3) computed from the bucket's reservation, rounded up to whole
// seconds.
func (l *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := l.keyer(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			now := l.now()
			limiter, ok := l.limiterFor(key, now)
			if !ok {
				writeRateLimited(w, int(limiterSweepEvery/time.Second))
				return
			}
			if !limiter.AllowN(now, 1) {
				// Reserve() reserves a token regardless of allow/deny, so
				// Cancel() right after puts it back: the 429 path consumes none.
				reservation := limiter.ReserveN(now, 1)
				wait := reservation.DelayFrom(now)
				reservation.CancelAt(now)
				writeRateLimited(w, retryAfterSeconds(wait))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
