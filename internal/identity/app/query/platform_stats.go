package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/cache"
)

// PlatformStatsView is the operator-dashboard at-a-glance shape.
// Deltas is non-nil only when PlatformStatsQuery.Window > 0.
type PlatformStatsView struct {
	TenantsTotal      int
	TenantsActive     int
	TenantsSuspended  int
	PersonsTotal      int
	MembershipsActive int
	// Deltas is non-nil only when PlatformStatsQuery.Window > 0.
	Deltas *PlatformStatsDeltas
}

// PlatformStatsDeltas carries "new rows in the last <window>" counts.
// Window is the human label ("24h"/"7d"/"30d") echoed from the request.
type PlatformStatsDeltas struct {
	Window            string // "24h" | "7d" | "30d"
	TenantsTotal      int    // tenants created in window
	TenantsActive     int    // tenants activated in window
	PersonsTotal      int    // persons created in window
	MembershipsActive int    // memberships that joined in window
}

// PlatformStatsQuery carries the optional delta-window param.
// Window == 0 skips deltas (base counts only); Window > 0 runs 4 extra COUNT(*) queries.
// WindowLabel is the human string echoed back on the response.
type PlatformStatsQuery struct {
	Window      time.Duration
	WindowLabel string
}

// ErrPlatformStatsInvalidWindow is returned when the delta window is not in
// {"", "24h", "7d", "30d"}. HTTP handler maps to 400.
var ErrPlatformStatsInvalidWindow = errors.New("platform_stats: invalid delta window")

// ParseDeltaWindow maps ?delta_window= to (duration, label).
// Empty string → 0 (no deltas). Closed set per ADR 0040.
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

// PlatformStatsBase is the 5-count base shape from [PlatformStatsReader.Base].
type PlatformStatsBase struct {
	TenantsTotal      int
	TenantsActive     int
	TenantsSuspended  int
	PersonsTotal      int
	MembershipsActive int
}

// PlatformStatsDeltaCounts is the 4-count delta shape from [PlatformStatsReader.Deltas].
type PlatformStatsDeltaCounts struct {
	TenantsTotal      int
	TenantsActive     int
	PersonsTotal      int
	MembershipsActive int
}

// PlatformStatsReader is the consumer-defined interface for cross-tenant
// dashboard reads (ADR 0047 boundary: no pgx/pgxpool/sqlc in app/).
// Both methods run under platform-scope tx so COUNT(*) on RLS-scoped
// tables sees every tenant's rows.
type PlatformStatsReader interface {
	Base(ctx context.Context) (PlatformStatsBase, error)
	Deltas(ctx context.Context, intervalLabel string) (PlatformStatsDeltaCounts, error)
}

// PlatformStatsHandler depends on [PlatformStatsReader] only (ADR 0047).
// Caching is applied externally via [CachedPlatformStatsHandler] so this
// handler stays a pure Postgres projection (ADR 0040 + ADR 0015).
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

// CachedPlatformStatsHandler wraps [PlatformStatsHandler] with cache.DashboardTTL.
// Keyed by delta_window label (closed set per ADR 0042).
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

// windowLabelToInterval maps the closed-set wire label to a Postgres interval string.
// Out-of-set labels pass through verbatim (caller already validated at HTTP boundary).
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
