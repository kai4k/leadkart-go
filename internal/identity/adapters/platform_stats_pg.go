// platform_stats_pg.go — pg-backed [query.PlatformStatsReader] (ADR 0047).
// Consumer-side interface: internal/identity/app/query/platform_stats.go.

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

// PlatformStatsReaderPG runs COUNT(*) rollups under platform scope so
// tenant_memberships (RLS+FORCE) is visible across all tenants.
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

var _ query.PlatformStatsReader = (*PlatformStatsReaderPG)(nil)

// Base runs the base rollup as a single round-trip so all counts share
// one snapshot.
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

// Deltas runs the "new rows in interval" rollup as a single round-trip.
// intervalLabel is a Postgres interval string ("24 hours", "7 days",
// "30 days"); the app layer validates it before calling here.
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

// parsePgInterval converts a "<n> hours|days" label into a pgtype.Interval
// whose field decomposition matches Postgres's own interval construction.
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
