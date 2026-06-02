package adapters

import (
	"time"

	"github.com/google/uuid"
)

// maxCursorTime returns the first-page sentinel for keyset pagination
// (ADR 0038). The predicate `(sort_value, id) < (cursor, cursor_id)`
// must match every row on page 1; year 9999 UTC satisfies that and
// fits in both PostgreSQL timestamptz and pgtype.Timestamptz bounds.
func maxCursorTime() time.Time {
	return time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
}

// maxCursorUUID returns the all-FF UUID used as the first-page
// tiebreak cursor; greater than every realistic UUIDv7.
func maxCursorUUID() uuid.UUID {
	return uuid.UUID{
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	}
}
