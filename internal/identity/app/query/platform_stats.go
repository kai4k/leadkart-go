package query

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/platform/pg"
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

// PlatformStatsHandler runs the dashboard query under TxScopePlatform
// so the COUNT(*) on tenant_memberships (RLS-scoped table) sees every
// tenant's rows — without the platform scope the connection's
// app.tenant_id GUC would filter to whatever tenant the JWT carries
// (a synthetic operator UUID with zero rows).
//
// Repository contracts aren't extended with COUNT methods because
// this is operator reporting, not domain state — keeping the SQL
// inline avoids three speculative methods with one caller.
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

// Handle returns the dashboard view. Five COUNT(*) queries inside a
// single TxScopePlatform read tx — the GUC bypass lets RLS-scoped
// tables (tenant_memberships) report a real cross-tenant count.
func (h PlatformStatsHandler) Handle(ctx context.Context) (PlatformStatsView, error) {
	var view PlatformStatsView
	queries := []struct {
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
		for _, q := range queries {
			if err := tx.QueryRow(ctx, q.sql).Scan(q.dst); err != nil {
				return fmt.Errorf("platform_stats: %s: %w", q.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return PlatformStatsView{}, err
	}
	return view, nil
}
