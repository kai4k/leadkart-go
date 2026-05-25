// Package cache wires the LeadKart HybridCache equivalent to .NET's
// `HybridCache` library: L1 in-process (ristretto) + L2 distributed
// (Redis) + singleflight stampede protection. Per ADR 0015 (revised).
//
// USAGE — typed facades only.
//
// Per `coding-standards.md` "Cache facade per concern" + `audit-checklist.md
// §12b`: every cached read path is wrapped in a typed facade. Raw
// HybridCache.Get / redis.Client.Get / ristretto.Cache.Get calls in
// handlers / repositories / queries are an audit finding. The arch
// test in [internal/common/cache/archtest_test.go] enforces this at
// build time.
//
// Citations: Microsoft Learn "Hybrid cache library in ASP.NET Core";
// Damian Edwards .NET 9 perf series; dgraph-io/ristretto README +
// production-use citations (Dgraph, Pinterest, Outline);
// golang.org/x/sync/singleflight godoc.
package cache

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	ristretto "github.com/dgraph-io/ristretto/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// HybridCache is the foundational cache: holds the L1 (ristretto)
// + L2 (Redis) + the singleflight group used by every typed facade.
//
// One HybridCache per process is the typical shape; facades get
// constructed against this single instance. Per the doctrine,
// HybridCache itself is NOT injected into business code — only
// the typed facades that wrap it are.
type HybridCache struct {
	L1     *ristretto.Cache[string, []byte]
	L2     redis.UniversalClient
	Group  *singleflight.Group
	Codec  Codec
	Logger *slog.Logger
}

// Config configures [New].
type Config struct {
	// L1MaxItems sets ristretto's MaxCost (entry count budget).
	// NumCounters defaults to 10× this per ristretto docs.
	L1MaxItems int64

	// L2 is the Redis client (go-redis/v9). MUST be non-nil; HybridCache
	// without L2 isn't a hybrid cache — wire a single-store cache
	// directly if that's what you want.
	L2 redis.UniversalClient

	// Codec serialises values for L2. Default JSON if nil.
	Codec Codec

	// Logger surfaces L1/L2 read/write errors. Default slog.Default.
	Logger *slog.Logger
}

// New constructs a HybridCache.
//
// L1 ristretto sizing per the README: NumCounters = 10 × MaxCost,
// MaxCost = L1MaxItems, BufferItems = 64.
func New(cfg Config) (*HybridCache, error) {
	if cfg.L2 == nil {
		return nil, errors.New("cache: L2 (redis client) required")
	}
	if cfg.L1MaxItems <= 0 {
		cfg.L1MaxItems = 10_000
	}
	if cfg.Codec == nil {
		cfg.Codec = JSONCodec{}
	}
	if cfg.Logger == nil {
		return nil, errors.New("cache: Logger required")
	}
	l1, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: cfg.L1MaxItems * 10,
		MaxCost:     cfg.L1MaxItems,
		BufferItems: 64,
	})
	if err != nil {
		return nil, fmt.Errorf("cache: ristretto: %w", err)
	}
	return &HybridCache{
		L1:     l1,
		L2:     cfg.L2,
		Group:  &singleflight.Group{},
		Codec:  cfg.Codec,
		Logger: cfg.Logger,
	}, nil
}

// Close releases resources. Safe to call on nil receiver.
func (h *HybridCache) Close() {
	if h == nil || h.L1 == nil {
		return
	}
	h.L1.Close()
}

// TTL groups L1 + L2 expiry plus optional jitter percent. Per
// ADR 0042 — per-use-case TTL profiles with documented rationale.
// Per-facade overrides via [WithTTL].
//
// JitterPercent applies to L2 only (L1 is per-process; no cross-
// replica stampede risk). 0 = no jitter; 10 = ±10% randomized. The
// jitter ranges from 0 to +JitterPercent — actual L2 TTL is
// baseL2 + rand(0, baseL2 * JitterPercent / 100). Enough to
// desynchronize replica expiries without making lifetime
// unpredictable for debugging.
type TTL struct {
	L1            time.Duration
	L2            time.Duration
	JitterPercent int
}

// Per-tier duration constants. Composed into [TTL] structs by the
// accessor functions below. Constants (vs package-level vars) make
// these genuinely immutable — a misbehaving caller can't redefine
// the security-stamp TTL at runtime.
//
// Profile choices follow ADR 0042 (research-grounded canon):
//   - Microsoft HybridCache typical pattern: 1 min L1 / 10 min L2.
//   - Auth0 / Okta session refresh: 30 sec for security-bearing.
//   - Stripe / Datadog dashboard caching: 5 min L2 with jitter.
//   - JWT-bound capabilities: 15 min L2 (stamp rotation invalidates).
//   - Search results: 5 min L2 with jitter (canonical Loki/Stripe).
const (
	defaultTTLL1 = 1 * time.Minute
	defaultTTLL2 = 5 * time.Minute

	// Auth0 / Okta session-validation refresh window. Tight on the
	// hot security path so revocation propagates within ~30s even
	// when the explicit invalidation cascade has a transient blip.
	securityStampTTL = 30 * time.Second

	// Capabilities profile — bound to (membership, security_stamp);
	// stamp rotation is the invalidation mechanism, so TTL is a
	// memory bound, not a freshness boundary.
	capabilitiesTTLL1 = 2 * time.Minute
	capabilitiesTTLL2 = 15 * time.Minute

	// Search-results profile — typing-burst tolerant; cross-replica
	// stampede mitigated by jitter.
	searchResultsTTLL1 = 30 * time.Second
	searchResultsTTLL2 = 5 * time.Minute

	// Dashboard profile — operator dashboards; slightly stale OK;
	// jitter mandatory at >1 replica.
	dashboardTTLL1 = 1 * time.Minute
	dashboardTTLL2 = 5 * time.Minute

	// Default jitter percent for cache profiles that use it. ±10%
	// is enough to desynchronize replica expiries without making
	// TTL behavior unpredictable for debugging.
	defaultJitterPercent = 10
)

// DefaultTTL is the standard L1+L2 retention per ADR 0015. Returned
// by-value so callers can't mutate a shared instance.
//
// Used by: generic reference data, lookups, configuration values.
// No jitter — minor TTL drift on commodity reads doesn't pay rent
// for the unpredictability cost.
func DefaultTTL() TTL { return TTL{L1: defaultTTLL1, L2: defaultTTLL2} }

// SecurityStampTTL is the Auth0/Okta session-validation refresh
// window — faster invalidation on the security-bearing freshness
// path. Returned by-value so callers can't mutate a shared instance.
//
// Used by: SecurityStampCache (the canonical caller). No jitter —
// single-tier (WithOmitL1) facade; no cross-replica stampede surface.
func SecurityStampTTL() TTL { return TTL{L1: securityStampTTL, L2: securityStampTTL} }

// CapabilitiesTTL is the per-(membership, security_stamp) cache
// profile for /v1/auth/me/capabilities profile enrichment. Per
// ADR 0042 — longer L2 (15min) because the security_stamp IS the
// invalidation mechanism; TTL is a memory bound, not a correctness
// one.
//
// No jitter — cache key is per-membership (low cross-replica
// collision rate); invalidation is event-driven via stamp rotation.
func CapabilitiesTTL() TTL { return TTL{L1: capabilitiesTTLL1, L2: capabilitiesTTLL2} }

// SearchResultsTTL is the cache profile for paginated search result
// lists (?q= queries). Per ADR 0042 — short L1 (typing-burst
// tolerant) + 5min L2 with jitter (canonical Stripe Dashboard /
// Loki Results Cache value).
//
// Jitter ±10% — search-keyed across many strokes per session; at
// >1 replica, identical TTLs would synchronize expiries. Jitter
// desynchronizes; mandatory.
func SearchResultsTTL() TTL {
	return TTL{L1: searchResultsTTLL1, L2: searchResultsTTLL2, JitterPercent: defaultJitterPercent}
}

// DashboardTTL is the cache profile for operator-dashboard counts /
// stats / deltas. Per ADR 0042 — 5min L2 with jitter; matches
// Datadog / Stripe Dashboard cadence.
//
// Jitter ±10% — operators refresh dashboards on similar wall-clock
// boundaries; identical TTLs would stampede.
func DashboardTTL() TTL {
	return TTL{L1: dashboardTTLL1, L2: dashboardTTLL2, JitterPercent: defaultJitterPercent}
}

// L2WithJitter returns the L2 duration with the configured jitter
// percent applied additively in the [0, +JitterPercent%] band. Per
// ADR 0042 — desynchronizes replica expiries without making lifetime
// unpredictable for debugging.
//
// Math: actualL2 = baseL2 + rand(0, baseL2 * JitterPercent / 100).
// JitterPercent == 0 returns baseL2 unchanged.
//
// math/rand/v2 (Go 1.22+) is goroutine-safe + seeded automatically;
// no rand.Seed setup needed.
func (t TTL) L2WithJitter() time.Duration {
	if t.JitterPercent <= 0 {
		return t.L2
	}
	maxBoost := int64(t.L2) * int64(t.JitterPercent) / 100
	if maxBoost <= 0 {
		return t.L2
	}
	// Intentionally math/rand/v2 (not crypto/rand): jitter is for
	// replica desynchronization, not security. crypto/rand here
	// would burn entropy on a non-security-sensitive code path.
	return t.L2 + time.Duration(rand.Int64N(maxBoost)) //nolint:gosec // G404: jitter is not security-sensitive
}
