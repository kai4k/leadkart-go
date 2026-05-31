package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
)

// UnverifiedContactReader is the pgx/sqlc read-model for
// [query.ListUnverifiedContactsReader]. unverified_contacts is platform-only
// (RLS uvc_platform_only USING app.is_platform()), so every read MUST run in
// a TxScopePlatform tx that binds the platform GUC: a bare pool checkout has
// is_platform()=false and FORCE RLS filters every row out. The GUC is
// tx-local, so middleware cannot set it for a later separate checkout.
type UnverifiedContactReader struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewUnverifiedContactReader wires the reader.
func NewUnverifiedContactReader(pool *pgxpool.Pool, tx *pg.Transactor) *UnverifiedContactReader {
	return &UnverifiedContactReader{pool: pool, tx: tx, q: db.New(pool)}
}

// ListUnverifiedContactsPage satisfies the app-layer interface. An empty
// cursor (first page) decodes to NULL on the SQL side.
func (r *UnverifiedContactReader) ListUnverifiedContactsPage(
	ctx context.Context,
	state string,
	cursor pagination.Cursor,
	pageSize int,
) ([]query.UnverifiedContactView, error) {
	cursorAt := pgtype.Timestamptz{}
	cursorID := pgtype.UUID{}
	if !cursor.SortValue.IsZero() && cursor.ID != "" {
		cursorAt = pgconv.PgTimestamp(cursor.SortValue)
		cursorID = pgUUIDOptStr(cursor.ID)
	}

	clamped := int32Clamp(pageSize)
	var out []query.UnverifiedContactView
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		rows, qerr := r.q.WithTx(tx).ListUnverifiedContactsPage(ctx, db.ListUnverifiedContactsPageParams{
			StateFilter: state,
			CursorAt:    cursorAt,
			CursorID:    cursorID,
			PageSize:    clamped,
		})
		if qerr != nil {
			return qerr
		}
		out = make([]query.UnverifiedContactView, 0, len(rows))
		for _, row := range rows {
			out = append(out, query.UnverifiedContactView{
				ID:                    pgconv.UUIDFromPg(row.ID).String(),
				State:                 row.State,
				ContactName:           row.ContactName,
				MobileE164:            row.MobileE164,
				City:                  row.City,
				StateGeo:              row.StateGeo,
				CreatedAt:             pgconv.TimeFromPg(row.CreatedAt).Format(time.RFC3339Nano),
				CreatedByMembershipID: pgconv.UUIDFromPg(row.CreatedByMembershipID).String(),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unverified contacts reader: list: %w", err)
	}
	return out, nil
}

// int32Clamp narrows an int to int32 in [1, 1<<30]. App-layer ClampPageSize
// already bounds [1, 200]; this guards the sqlc int32 cast against overflow
// (gosec).
func int32Clamp(n int) int32 {
	if n <= 0 {
		return 1
	}
	if n > 1<<30 {
		return 1 << 30
	}
	return int32(n)
}
