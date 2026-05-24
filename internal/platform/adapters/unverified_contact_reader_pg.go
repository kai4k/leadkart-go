package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
)

// UnverifiedContactReader is the pgx/sqlc-backed read-model satisfying
// [query.ListUnverifiedContactsReader]. Platform-only — runs under the
// caller's existing scope (the HTTP layer authn middleware sets the
// platform GUC when the operator's JWT is_platform=true).
type UnverifiedContactReader struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewUnverifiedContactReader wires the reader.
func NewUnverifiedContactReader(pool *pgxpool.Pool, tx *pg.Transactor) *UnverifiedContactReader {
	return &UnverifiedContactReader{pool: pool, tx: tx, q: db.New(pool)}
}

// ListUnverifiedContactsPage satisfies the app-layer interface. The
// cursor is decoded into (sort_value, id) at the boundary; empty cursor
// (first page) translates to NULL on the SQL side.
func (r *UnverifiedContactReader) ListUnverifiedContactsPage(
	ctx context.Context,
	state string,
	cursor pagination.Cursor,
	pageSize int,
) ([]query.UnverifiedContactView, error) {
	cursorAt := pgtype.Timestamptz{}
	cursorID := pgtype.UUID{}
	if !cursor.SortValue.IsZero() && cursor.ID != "" {
		cursorAt = pgtype.Timestamptz{Time: cursor.SortValue.UTC(), Valid: true}
		cursorID = pgUUIDOptStr(cursor.ID)
	}

	clamped := int32Clamp(pageSize)
	rows, err := r.q.ListUnverifiedContactsPage(ctx, db.ListUnverifiedContactsPageParams{
		StateFilter: state,
		CursorAt:    cursorAt,
		CursorID:    cursorID,
		PageSize:    clamped,
	})
	if err != nil {
		return nil, fmt.Errorf("unverified contacts reader: list: %w", err)
	}
	out := make([]query.UnverifiedContactView, 0, len(rows))
	for _, row := range rows {
		out = append(out, query.UnverifiedContactView{
			ID:                    uuidFromPg(row.ID).String(),
			State:                 row.State,
			ContactName:           row.ContactName,
			MobileE164:            row.MobileE164,
			City:                  row.City,
			StateGeo:              row.StateGeo,
			CreatedAt:             timeFromPg(row.CreatedAt).Format(time.RFC3339Nano),
			CreatedByMembershipID: uuidFromPg(row.CreatedByMembershipID).String(),
		})
	}
	return out, nil
}

// int32Clamp narrows an int to int32, clamping to [1, MaxInt32]. The
// app-layer ClampPageSize already bounds [1, 200], so this is a defence
// against overflow when sqlc demands int32 + gosec flags the raw cast.
func int32Clamp(n int) int32 {
	if n <= 0 {
		return 1
	}
	if n > 1<<30 {
		return 1 << 30
	}
	return int32(n)
}
