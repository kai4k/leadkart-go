package adapters

import (
	"time"

	"github.com/google/uuid"
)

// maxCursorTime returns the sentinel "infinity" timestamp used as the
// first-page cursor for keyset pagination per ADR 0038.
//
// The keyset predicate is `(sort_value, id) < (cursor_sort, cursor_id)`;
// the first page needs a cursor that matches every row. Far-future UTC
// is chosen so the predicate is strictly less than every realistic
// row's created_at.
//
// Year 9999 stays inside both PostgreSQL `timestamptz` (4713 BC – 294276
// AD) and `pgtype.Timestamptz` round-trip bounds.
func maxCursorTime() time.Time {
	return time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
}

// maxCursorUUID returns the sentinel "all-FF" UUID used as the
// first-page tiebreak cursor. Greater than every realistic UUIDv7.
func maxCursorUUID() uuid.UUID {
	return uuid.UUID{
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	}
}
