package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/cache"
)

// PlatformStatsView is the operator-dashboard at-a-glance shape.
//
// Counts are point-in-time totals; Deltas (when populated) carry
// "new rows since now() - window" for each metric — the standard
// "Δ in last 24h" dashboard widget shape used by Stripe / Datadog /
// Linear. Populated only when the request specifies a window; nil
// when no delta was asked for.
type PlatformStatsView struct {
	TenantsTotal      int
	TenantsActive     int
	TenantsSuspended  int
	PersonsTotal      int
	MembershipsActive int
	// Deltas is non-nil only when PlatformStatsQuery.Window > 0.
	Deltas *PlatformStatsDeltas
}

// PlatformStatsDeltas carries the "new rows in the last <window>"
// counts that pair with each base metric. Window is the human label
// ("24h" / "7d" / "30d") that the caller supplied; the actual
// duration that produced these counts is exactly that label's
// canonical mapping.
type PlatformStatsDeltas struct {
	Window            string // "24h" | "7d" | "30d"
	TenantsTotal      int    // tenants created in window
	TenantsActive     int    // tenants activated in window
	PersonsTotal      int    // persons created in window
	MembershipsActive int    // memberships that joined in window
}

// PlatformStatsQuery carries the optional delta-window param.
// Window == 0 means "skip the delta queries" (5 base counts only).
// Window > 0 triggers an additional 4 COUNT(*) queries for the
// "new since now() - Window" deltas. WindowLabel is the human
// string ("24h"/"7d"/"30d") echoed back on the response.
type PlatformStatsQuery struct {
	Window      time.Duration
	WindowLabel string
}

// ErrPlatformStatsInvalidWindow surfaces when the supplied window
// isn't in the closed set {"", "24h", "7d", "30d"}. HTTP handler
// maps to 400.
var ErrPlatformStatsInvalidWindow = errors.New("platform_stats: invalid delta window")

// ParseDeltaWindow maps a wire ?delta_window= string to the
// canonical Duration + label. Empty string returns 0 (no deltas).
// Per ADR 0040 / cache-key-explosion rule: closed set only.
func ParseDeltaWindow(s string) (time.Duration, string, error) {
	switch s {
	case "":
		return 0, "", nil
	case "24h":
		return 24 * time.Hour, "24h", nil
	case "7d":
		return 7 * 24 * time.Hour, "7d", nil
	case "30d":
		return 30 * 24 * time.Hour, "30d", nil
	default:
		return 0, "", fmt.Errorf("%w: %q (allowed: 24h, 7d, 30d)", ErrPlatformStatsInvalidWindow, s)
	}
}

// PlatformStatsBase is the 5-count base shape returned by
// [PlatformStatsReader.Base]. App layer composes [PlatformStatsView]
// from this (+ optional [PlatformStatsDeltas] from the deltas branch).
type PlatformStatsBase struct {
	TenantsTotal      int
	TenantsActive     int
	TenantsSuspended  int
	PersonsTotal      int
	MembershipsActive int
}

// PlatformStatsDeltaCounts is the 4-count delta shape returned by
// [PlatformStatsReader.Deltas]. Window-label sidecar lives in the
// caller; the adapter just answers the "rows in last interval" question.
type PlatformStatsDeltaCounts struct {
	TenantsTotal      int
	TenantsActive     int
	PersonsTotal      int
	MembershipsActive int
}

// PlatformStatsReader is the consumer-defined contract for cross-
// tenant dashboard reads. Defined here next to its sole consumer
// [PlatformStatsHandler]; concrete pg-backed implementation lives in
// internal/identity/adapters/ where db.* / pgx.* are permitted.
// App layer carries NO pgx, pgxpool, or sqlc imports.
//
// Both methods run under platform-scope tx so the COUNT(*) on
// tenant_memberships (RLS-scoped) sees every tenant's rows.
type PlatformStatsReader interface {
	Base(ctx context.Context) (PlatformStatsBase, error)
	Deltas(ctx context.Context, intervalLabel string) (PlatformStatsDeltaCounts, error)
}

// PlatformStatsHandler depends on [PlatformStatsReader] only —
// boundary discipline per ADR 0047.
//
// Cache discipline (ADR 0040 + ADR 0015 HybridCache canon):
//
//   - This handler computes from-scratch every call. Caching wraps the
//     handler from the OUTSIDE — the HTTP boundary layer holds the
//     HybridCache facade keyed by (delta_window) with a 5min TTL.
//   - Reason for cache-outside-handler: the handler stays a pure
//     read-from-Postgres function, easier to test + integration-
//     verify. Cache is a side-channel that doesn't change the
//     handler's contract.
type PlatformStatsHandler struct {
	reader PlatformStatsReader
}

// NewPlatformStatsHandler wires the handler.
func NewPlatformStatsHandler(reader PlatformStatsReader) PlatformStatsHandler {
	if reader == nil {
		panic("query: NewPlatformStatsHandler reader required")
	}
	return PlatformStatsHandler{reader: reader}
}

// CachedPlatformStatsHandler is the cache-wrapped facade in front of
// the un-cached [PlatformStatsHandler]. Per ADR 0042 — keyed by
// delta_window label ("" / "24h" / "7d" / "30d" — closed set);
// uses [cache.DashboardTTL] (1min L1 / 5min L2 + ±10% jitter).
type CachedPlatformStatsHandler struct {
	facade *cache.Facade[string, PlatformStatsView]
}

// NewCachedPlatformStatsHandler builds the facade-wrapped handler.
func NewCachedPlatformStatsHandler(inner PlatformStatsHandler, hc *cache.HybridCache) CachedPlatformStatsHandler {
	if hc == nil {
		panic("query: NewCachedPlatformStatsHandler hybrid cache required")
	}
	facade := cache.NewFacade[string, PlatformStatsView](
		hc, "platform-stats",
		func(k string) string { return "leadkart:platform-stats:window=" + k },
		func(ctx context.Context, k string) (PlatformStatsView, error) {
			dur, label, err := ParseDeltaWindow(k)
			if err != nil {
				return PlatformStatsView{}, fmt.Errorf("platform_stats: cached facade: invalid window key %q: %w", k, err)
			}
			return inner.Handle(ctx, PlatformStatsQuery{Window: dur, WindowLabel: label})
		},
		cache.WithTTL(cache.DashboardTTL()),
	)
	return CachedPlatformStatsHandler{facade: facade}
}

// Handle returns the cached dashboard view.
func (h CachedPlatformStatsHandler) Handle(ctx context.Context, q PlatformStatsQuery) (PlatformStatsView, error) {
	return h.facade.Get(ctx, q.WindowLabel)
}

// Handle returns the dashboard view.
func (h PlatformStatsHandler) Handle(ctx context.Context, q PlatformStatsQuery) (PlatformStatsView, error) {
	base, err := h.reader.Base(ctx)
	if err != nil {
		return PlatformStatsView{}, fmt.Errorf("platform_stats: base: %w", err)
	}
	view := PlatformStatsView{
		TenantsTotal:      base.TenantsTotal,
		TenantsActive:     base.TenantsActive,
		TenantsSuspended:  base.TenantsSuspended,
		PersonsTotal:      base.PersonsTotal,
		MembershipsActive: base.MembershipsActive,
	}
	if q.Window <= 0 {
		return view, nil
	}

	intervalStr := windowLabelToInterval(q.WindowLabel)
	dc, err := h.reader.Deltas(ctx, intervalStr)
	if err != nil {
		return PlatformStatsView{}, fmt.Errorf("platform_stats: deltas: %w", err)
	}
	view.Deltas = &PlatformStatsDeltas{
		Window:            q.WindowLabel,
		TenantsTotal:      dc.TenantsTotal,
		TenantsActive:     dc.TenantsActive,
		PersonsTotal:      dc.PersonsTotal,
		MembershipsActive: dc.MembershipsActive,
	}
	return view, nil
}

// windowLabelToInterval maps the closed-set wire label to a Postgres
// interval string. Out-of-set labels fall through verbatim — the
// caller validated at the HTTP boundary, so this is the trust edge.
func windowLabelToInterval(label string) string {
	switch label {
	case "24h":
		return "24 hours"
	case "7d":
		return "7 days"
	case "30d":
		return "30 days"
	default:
		return label
	}
}
