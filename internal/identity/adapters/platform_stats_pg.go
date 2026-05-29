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
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
)

// PlatformStatsReaderPG runs the dashboard COUNT(*) rollups under
// platform scope so RLS-scoped tenant_memberships sees every tenant.
type PlatformStatsReaderPG struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewPlatformStatsReaderPG wires the adapter.
func NewPlatformStatsReaderPG(pool *pgxpool.Pool, tx *pg.Transactor) *PlatformStatsReaderPG {
	if pool == nil {
		panic("adapters: NewPlatformStatsReaderPG pool required")
	}
	if tx == nil {
		panic("adapters: NewPlatformStatsReaderPG transactor required")
	}
	return &PlatformStatsReaderPG{pool: pool, tx: tx, q: db.New(pool)}
}

// Compile-time interface satisfaction.
var _ query.PlatformStatsReader = (*PlatformStatsReaderPG)(nil)

// Base runs the base rollup as ONE round-trip (scalar subqueries)
// inside a platform-scope tx so the counts share one snapshot.
func (r *PlatformStatsReaderPG) Base(ctx context.Context) (query.PlatformStatsBase, error) {
	var b query.PlatformStatsBase
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		row, qerr := r.q.WithTx(tx).PlatformStatsBase(ctx)
		if qerr != nil {
			return fmt.Errorf("platform stats base: %w", qerr)
		}
		b = query.PlatformStatsBase{
			TenantsTotal:      int(row.TenantsTotal),
			TenantsActive:     int(row.TenantsActive),
			TenantsSuspended:  int(row.TenantsSuspended),
			PersonsTotal:      int(row.PersonsTotal),
			MembershipsActive: int(row.MembershipsActive),
		}
		return nil
	})
	if err != nil {
		return query.PlatformStatsBase{}, err
	}
	return b, nil
}

// Deltas runs the "new rows in interval" rollup as ONE round-trip.
// intervalLabel is a Postgres interval string ("24 hours" / "7 days" /
// "30 days"). The closed-set label validator lives in the app layer;
// this adapter trusts the input.
func (r *PlatformStatsReaderPG) Deltas(ctx context.Context, intervalLabel string) (query.PlatformStatsDeltaCounts, error) {
	win, err := parsePgInterval(intervalLabel)
	if err != nil {
		return query.PlatformStatsDeltaCounts{}, fmt.Errorf("platform stats deltas: %w", err)
	}
	var d query.PlatformStatsDeltaCounts
	err = r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		row, qerr := r.q.WithTx(tx).PlatformStatsDeltas(ctx, win)
		if qerr != nil {
			return fmt.Errorf("platform stats deltas: %w", qerr)
		}
		d = query.PlatformStatsDeltaCounts{
			TenantsTotal:      int(row.TenantsTotal),
			TenantsActive:     int(row.TenantsActive),
			PersonsTotal:      int(row.PersonsTotal),
			MembershipsActive: int(row.MembershipsActive),
		}
		return nil
	})
	if err != nil {
		return query.PlatformStatsDeltaCounts{}, err
	}
	return d, nil
}

// parsePgInterval maps the closed-set "<n> hours|days" label
// (produced by windowLabelToInterval in the app layer) into a
// pgtype.Interval whose field decomposition matches Postgres's own
// `interval '<n> days'` / `interval '<n> hours'` construction exactly —
// preserving the prior `now() - $1::interval` semantics.
func parsePgInterval(label string) (pgtype.Interval, error) {
	n, unit, ok := strings.Cut(label, " ")
	if !ok {
		return pgtype.Interval{}, fmt.Errorf("malformed interval label %q", label)
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil {
		return pgtype.Interval{}, fmt.Errorf("malformed interval count %q: %w", label, err)
	}
	switch strings.TrimSuffix(unit, "s") {
	case "hour":
		return pgtype.Interval{Microseconds: v * 3600 * 1_000_000, Valid: true}, nil
	case "day":
		//nolint:gosec // closed-set window labels: 1..30
		return pgtype.Interval{Days: int32(v), Valid: true}, nil
	default:
		return pgtype.Interval{}, fmt.Errorf("unsupported interval unit %q", unit)
	}
}
