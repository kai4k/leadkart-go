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
// test in [internal/platform/cache/archtest_test.go] enforces this at
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
		cfg.Logger = slog.Default()
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

// TTL groups L1 + L2 expiry. Per ADR 0015 default 1 min L1 / 5 min L2;
// per-facade overrides via [WithTTL].
type TTL struct {
	L1 time.Duration
	L2 time.Duration
}

// DefaultTTL — global default per ADR 0015.
var DefaultTTL = TTL{
	L1: 1 * time.Minute,
	L2: 5 * time.Minute,
}

// SecurityStampTTL — Auth0/Okta session-validation refresh window.
// Faster invalidation on the hot security path.
var SecurityStampTTL = TTL{
	L1: 30 * time.Second,
	L2: 30 * time.Second,
}
