package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/platform/pg"
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

// PlatformStatsHandler runs the dashboard query under TxScopePlatform
// so the COUNT(*) on tenant_memberships (RLS-scoped table) sees every
// tenant's rows — without the platform scope the connection's
// app.tenant_id GUC would filter to whatever tenant the JWT carries
// (a synthetic operator UUID with zero rows).
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
//
// Repository contracts aren't extended with COUNT methods because
// this is operator reporting, not domain state — keeping the SQL
// inline avoids speculative methods with one caller.
type PlatformStatsHandler struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
}

// NewPlatformStatsHandler wires the handler. tx is required to lift
// the connection into TxScopePlatform for the cross-tenant counts.
func NewPlatformStatsHandler(pool *pgxpool.Pool, tx *pg.Transactor) PlatformStatsHandler {
	if pool == nil {
		panic("query: NewPlatformStatsHandler pool required")
	}
	if tx == nil {
		panic("query: NewPlatformStatsHandler transactor required")
	}
	return PlatformStatsHandler{pool: pool, tx: tx}
}

// Handle returns the dashboard view.
//
// Base: 5 COUNT(*) queries inside a single TxScopePlatform read tx.
// With deltas: +4 COUNT(*) queries with WHERE created_at > now() -
// window (or activated_at / joined_at for the lifecycle-specific
// counts). Both branches reuse the same tx so the row counts are
// consistent against the same snapshot.
func (h PlatformStatsHandler) Handle(ctx context.Context, q PlatformStatsQuery) (PlatformStatsView, error) {
	var view PlatformStatsView
	base := []struct {
		name string
		sql  string
		dst  *int
	}{
		{"tenants_total", `SELECT COUNT(*) FROM identity.tenants`, &view.TenantsTotal},
		{"tenants_active", `SELECT COUNT(*) FROM identity.tenants WHERE status = 'active'`, &view.TenantsActive},
		{"tenants_suspended", `SELECT COUNT(*) FROM identity.tenants WHERE status = 'suspended'`, &view.TenantsSuspended},
		{"persons_total", `SELECT COUNT(*) FROM identity.persons WHERE NOT is_anonymised`, &view.PersonsTotal},
		{"memberships_active", `SELECT COUNT(*) FROM identity.tenant_memberships WHERE status = 'active'`, &view.MembershipsActive},
	}

	err := h.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		for _, query := range base {
			if err := tx.QueryRow(ctx, query.sql).Scan(query.dst); err != nil {
				return fmt.Errorf("platform_stats: %s: %w", query.name, err)
			}
		}
		if q.Window <= 0 {
			return nil
		}

		// Deltas branch — 4 extra COUNT(*) queries with a created/
		// activated/joined-in-window predicate. The lifecycle-specific
		// timestamps (activated_at, joined_at) are NULLable on rows
		// that haven't yet been activated/joined; COUNT(*) treats NULL
		// timestamps as "out of window" (the comparison evaluates to
		// NULL → row excluded).
		deltas := &PlatformStatsDeltas{Window: q.WindowLabel}
		view.Deltas = deltas
		deltaQueries := []struct {
			name string
			sql  string
			dst  *int
		}{
			{"delta_tenants_total", `SELECT COUNT(*) FROM identity.tenants WHERE created_at > now() - $1::interval`, &deltas.TenantsTotal},
			{"delta_tenants_active", `SELECT COUNT(*) FROM identity.tenants WHERE activated_at > now() - $1::interval`, &deltas.TenantsActive},
			{"delta_persons_total", `SELECT COUNT(*) FROM identity.persons WHERE created_at > now() - $1::interval AND NOT is_anonymised`, &deltas.PersonsTotal},
			{"delta_memberships_active", `SELECT COUNT(*) FROM identity.tenant_memberships WHERE joined_at > now() - $1::interval AND status = 'active'`, &deltas.MembershipsActive},
		}
		// Postgres accepts a string like "24 hours" as an interval cast.
		// Format the duration into a Postgres-friendly form via the
		// human-readable label rather than time.Duration.String() (which
		// can emit microseconds for fractional values).
		var intervalStr string
		switch q.WindowLabel {
		case "24h":
			intervalStr = "24 hours"
		case "7d":
			intervalStr = "7 days"
		case "30d":
			intervalStr = "30 days"
		default:
			intervalStr = q.WindowLabel
		}
		for _, query := range deltaQueries {
			if err := tx.QueryRow(ctx, query.sql, intervalStr).Scan(query.dst); err != nil {
				return fmt.Errorf("platform_stats: %s: %w", query.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return PlatformStatsView{}, err
	}
	return view, nil
}
