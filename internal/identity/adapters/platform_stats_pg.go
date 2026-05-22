// platform_stats_pg.go — concrete pg-backed [query.PlatformStatsReader].
//
// Lives in the adapters package where pgx + sqlc are permitted per
// ADR 0047 boundary discipline. The consumer-side interface
// [query.PlatformStatsReader] is defined in
// internal/identity/app/query/platform_stats.go.

package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// PlatformStatsReaderPG runs the dashboard COUNT(*) queries under
// platform scope so RLS-scoped tenant_memberships sees every tenant.
type PlatformStatsReaderPG struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
}

// NewPlatformStatsReaderPG wires the adapter.
func NewPlatformStatsReaderPG(pool *pgxpool.Pool, tx *pg.Transactor) *PlatformStatsReaderPG {
	if pool == nil {
		panic("adapters: NewPlatformStatsReaderPG pool required")
	}
	if tx == nil {
		panic("adapters: NewPlatformStatsReaderPG transactor required")
	}
	return &PlatformStatsReaderPG{pool: pool, tx: tx}
}

// Compile-time interface satisfaction.
var _ query.PlatformStatsReader = (*PlatformStatsReaderPG)(nil)

// Base runs the 5 COUNT(*) queries inside a single platform-scope tx
// so the row counts are consistent against the same snapshot.
func (r *PlatformStatsReaderPG) Base(ctx context.Context) (query.PlatformStatsBase, error) {
	var b query.PlatformStatsBase
	rows := []struct {
		name string
		sql  string
		dst  *int
	}{
		{"tenants_total", `SELECT COUNT(*) FROM identity.tenants`, &b.TenantsTotal},
		{"tenants_active", `SELECT COUNT(*) FROM identity.tenants WHERE status = 'active'`, &b.TenantsActive},
		{"tenants_suspended", `SELECT COUNT(*) FROM identity.tenants WHERE status = 'suspended'`, &b.TenantsSuspended},
		{"persons_total", `SELECT COUNT(*) FROM identity.persons WHERE NOT is_anonymised`, &b.PersonsTotal},
		{"memberships_active", `SELECT COUNT(*) FROM identity.tenant_memberships WHERE status = 'active'`, &b.MembershipsActive},
	}
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range rows {
			if err := tx.QueryRow(ctx, row.sql).Scan(row.dst); err != nil {
				return fmt.Errorf("%s: %w", row.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return query.PlatformStatsBase{}, err
	}
	return b, nil
}

// Deltas runs the 4 "new rows in interval" COUNT(*) queries.
// intervalLabel is a Postgres interval string ("24 hours" / "7 days" /
// "30 days"). The label validator lives in the app layer; this
// adapter trusts the input.
func (r *PlatformStatsReaderPG) Deltas(ctx context.Context, intervalLabel string) (query.PlatformStatsDeltaCounts, error) {
	var d query.PlatformStatsDeltaCounts
	rows := []struct {
		name string
		sql  string
		dst  *int
	}{
		{"delta_tenants_total", `SELECT COUNT(*) FROM identity.tenants WHERE created_at > now() - $1::interval`, &d.TenantsTotal},
		{"delta_tenants_active", `SELECT COUNT(*) FROM identity.tenants WHERE activated_at > now() - $1::interval`, &d.TenantsActive},
		{"delta_persons_total", `SELECT COUNT(*) FROM identity.persons WHERE created_at > now() - $1::interval AND NOT is_anonymised`, &d.PersonsTotal},
		{"delta_memberships_active", `SELECT COUNT(*) FROM identity.tenant_memberships WHERE joined_at > now() - $1::interval AND status = 'active'`, &d.MembershipsActive},
	}
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range rows {
			if err := tx.QueryRow(ctx, row.sql, intervalLabel).Scan(row.dst); err != nil {
				return fmt.Errorf("%s: %w", row.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return query.PlatformStatsDeltaCounts{}, err
	}
	return d, nil
}
