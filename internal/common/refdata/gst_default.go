// Package refdata holds platform-wide reference-data readers per
// BRD §8.5 — owned by BuildingBlocks/Infrastructure/ReferenceData
// (the LeadKart shared kernel). Read-only for tenants; writes happen
// via SuperAdmin-tier admin endpoints (out of scope for this slice).
//
// Slice 1: GstDefaultReader. Future readers (pincode lookup, state
// list, dosage-form catalogue) land alongside.
package refdata

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/cache"
)

// GstDefaultReader resolves the default GST rate (basis points) for a
// pharma product category per BRD Appendix C.5. Returns
// (rate, true, nil) on hit, (0, false, nil) on miss, (0, false, err)
// on infrastructure failure.
type GstDefaultReader interface {
	Default(ctx context.Context, category string) (int, bool, error)
}

// GstDefaultReaderPG is the pgx-backed implementation. Cached via the
// supplied [cache.Facade] (long TTL — reference data changes rarely;
// the v0.2 SuperAdmin write endpoint will explicitly Invalidate on
// edit when shipped).
type GstDefaultReaderPG struct {
	pool  *pgxpool.Pool
	cache *cache.Facade[string, gstResult]
}

// NewGstDefaultReaderPG wires the reader. The factory closure on the
// underlying [cache.Facade] runs the actual DB lookup on miss.
//
// TTL profile: per ADR 0042 — reference data uses the Default profile
// (1m L1 / 5m L2). SuperAdmin writes will invalidate explicitly when
// the write endpoint ships.
func NewGstDefaultReaderPG(pool *pgxpool.Pool, hybrid *cache.HybridCache) *GstDefaultReaderPG {
	r := &GstDefaultReaderPG{pool: pool}
	if hybrid != nil {
		r.cache = cache.NewFacade(hybrid, "refdata.gst_default",
			func(category string) string { return "refdata:gst_default:" + category },
			func(ctx context.Context, key string) (gstResult, error) {
				rate, found, err := r.loadDefault(ctx, key)
				return gstResult{Rate: rate, Found: found}, err
			},
			cache.WithTTL(cache.DefaultTTL()),
		)
	}
	return r
}

// gstResult is the cached projection. Found is needed because the
// cache stores the "no row" answer too — avoids hammering the DB on
// repeated lookups for unseeded categories.
type gstResult struct {
	Rate  int
	Found bool
}

// Default looks up the per-category default GST rate.
func (r *GstDefaultReaderPG) Default(ctx context.Context, category string) (int, bool, error) {
	if category == "" {
		return 0, false, nil
	}
	if r.cache == nil {
		return r.loadDefault(ctx, category)
	}
	res, err := r.cache.Get(ctx, category)
	if err != nil {
		return 0, false, err
	}
	return res.Rate, res.Found, nil
}

// loadDefault hits the DB. Runs as platform-scope (no GUC binding) —
// shared.* has no RLS by design.
func (r *GstDefaultReaderPG) loadDefault(ctx context.Context, category string) (int, bool, error) {
	var rate int
	err := r.pool.QueryRow(ctx,
		`SELECT default_gst_rate_bps
		 FROM   shared.product_category_gst_defaults
		 WHERE  category = $1`,
		category,
	).Scan(&rate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("refdata: query gst default: %w", err)
	}
	return rate, true, nil
}
