package adapters

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/companysettings/adapters/db"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// SettingsHistoryReader is the pgx/sqlc-backed [query.HistoryReader]: the
// append-only policy history, newest version first. Tenant-scoped
// (TxScopeTenant semantics: the GUC is bound from the explicit tenant id).
type SettingsHistoryReader struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewSettingsHistoryReader wires the reader.
func NewSettingsHistoryReader(pool *pgxpool.Pool, tx *pg.Transactor) *SettingsHistoryReader {
	return &SettingsHistoryReader{pool: pool, tx: tx, q: db.New(pool)}
}

var _ query.HistoryReader = (*SettingsHistoryReader)(nil)

// ListHistory pages the history by version; the cursor id carries the version
// of the last row of the previous page.
func (r *SettingsHistoryReader) ListHistory(ctx context.Context, tenantID tenant.ID, cursor pagination.Cursor, pageSize int) (pagination.Page[query.HistoryEntry], error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return pagination.Page[query.HistoryEntry]{}, fmt.Errorf("settings history: parse tenant id: %w", err)
	}
	before := int64(math.MaxInt64)
	if cursor.ID != "" {
		v, perr := strconv.ParseInt(cursor.ID, 10, 64)
		if perr != nil || v < 1 {
			return pagination.Page[query.HistoryEntry]{}, fmt.Errorf("%w: version", pagination.ErrInvalidCursor)
		}
		before = v
	}
	var rows []db.CompanysettingsPolicyHistory
	run := func(ctx context.Context, tx pgx.Tx) error {
		got, qerr := r.q.WithTx(tx).ListPolicyHistoryPage(ctx, db.ListPolicyHistoryPageParams{
			TenantID: pgconv.PgUUID(tid), Version: before, Limit: int32(pageSize + 1), //nolint:gosec // pageSize is clamped to <= 200
		})
		rows = got
		return qerr
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		err = run(ctx, tx)
	} else {
		err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), run)
	}
	if err != nil {
		return pagination.Page[query.HistoryEntry]{}, fmt.Errorf("settings history: list: %w", err)
	}
	items := make([]query.HistoryEntry, 0, len(rows))
	for _, row := range rows {
		pol, derr := commercepolicy.DecodeStored(row.Policy)
		if derr != nil {
			return pagination.Page[query.HistoryEntry]{}, fmt.Errorf("settings history: stored policy: %w", derr)
		}
		e := query.HistoryEntry{
			Version: row.Version, Policy: pol, ChangedKeys: row.ChangedKeys, Reason: row.Reason,
			ChangedAt: pgconv.TimeFromPg(row.ChangedAt),
		}
		if row.ActorMembershipID.Valid {
			e.ActorMembershipID = pgconv.UUIDFromPg(row.ActorMembershipID).String()
		}
		if row.ImpersonatorOperatorID != nil {
			e.ImpersonatorOperatorID = *row.ImpersonatorOperatorID
		}
		items = append(items, e)
	}
	return pagination.BuildPage(items, pageSize, func(e query.HistoryEntry) pagination.Cursor {
		return pagination.Cursor{SortValue: e.ChangedAt, ID: strconv.FormatInt(e.Version, 10)}
	}), nil
}
