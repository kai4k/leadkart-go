package query

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PlatformStatsView is the operator-dashboard at-a-glance shape.
// Single round-trip — three counts via UNION ALL or three separate
// queries; we use three separate queries for clarity (Postgres
// optimises individual COUNT(*) over indexed predicates fast).
type PlatformStatsView struct {
	TenantsTotal      int
	TenantsActive     int
	TenantsSuspended  int
	PersonsTotal      int
	MembershipsActive int
}

// PlatformStatsHandler runs the dashboard query directly off pgxpool
// rather than through the Repository contracts — this is operator
// reporting, not domain state. Keeping the SQL inline avoids
// extending three different repos with COUNT methods that have one
// caller.
type PlatformStatsHandler struct {
	pool *pgxpool.Pool
}

// NewPlatformStatsHandler wires the handler.
func NewPlatformStatsHandler(pool *pgxpool.Pool) PlatformStatsHandler {
	if pool == nil {
		panic("query: NewPlatformStatsHandler pool required")
	}
	return PlatformStatsHandler{pool: pool}
}

// Handle returns the dashboard view. Each row counts in its own
// query — five round-trips is acceptable for a non-hot operator
// dashboard. If this surfaces in profiling, swap for a single
// UNION ALL query.
func (h PlatformStatsHandler) Handle(ctx context.Context) (PlatformStatsView, error) {
	queries := []struct {
		name string
		sql  string
		dst  *int
	}{}
	var view PlatformStatsView
	queries = append(queries,
		struct {
			name string
			sql  string
			dst  *int
		}{"tenants_total", `SELECT COUNT(*) FROM identity.tenants`, &view.TenantsTotal},
		struct {
			name string
			sql  string
			dst  *int
		}{"tenants_active", `SELECT COUNT(*) FROM identity.tenants WHERE status = 'active'`, &view.TenantsActive},
		struct {
			name string
			sql  string
			dst  *int
		}{"tenants_suspended", `SELECT COUNT(*) FROM identity.tenants WHERE status = 'suspended'`, &view.TenantsSuspended},
		struct {
			name string
			sql  string
			dst  *int
		}{"persons_total", `SELECT COUNT(*) FROM identity.persons WHERE NOT is_anonymised`, &view.PersonsTotal},
		struct {
			name string
			sql  string
			dst  *int
		}{"memberships_active", `SELECT COUNT(*) FROM identity.tenant_memberships WHERE status = 'active' AND NOT is_deleted`, &view.MembershipsActive},
	)
	for _, q := range queries {
		if err := h.pool.QueryRow(ctx, q.sql).Scan(q.dst); err != nil {
			return PlatformStatsView{}, fmt.Errorf("platform_stats: %s: %w", q.name, err)
		}
	}
	return view, nil
}
