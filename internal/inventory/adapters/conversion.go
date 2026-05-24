// Package adapters holds Inventory outbound concrete impls — pgx + sqlc
// repository structs that satisfy the domain repository interfaces.
//
// Mirror of internal/identity/adapters layout per TDL canon.
package adapters

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// pgUUID wraps a uuid.UUID into pgtype.UUID for sqlc query parameters.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// pgRequiredTimestamp wraps a non-nullable timestamp; always Valid.
// Slice 1 has no nullable timestamps yet — every inventory timestamp
// is NOT NULL on the schema (created_at, updated_at, occurred_at,
// deleted_at conditional on is_deleted). Once a nullable timestamp
// surfaces, add a pgTimestamp helper alongside.
func pgRequiredTimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// pgDate wraps a date-only time.Time into pgtype.Date (sqlc maps SQL
// `date` columns to pgtype.Date). Inventory stores manufacture_date +
// expiry_date as SQL `date`, so the adapter strips time-of-day +
// timezone — the domain canonicalises to UTC at construction.
func pgDate(t time.Time) pgtype.Date {
	if t.IsZero() {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: t.UTC(), Valid: true}
}

// uuidFromPg unwraps a pgtype.UUID into uuid.UUID. Invalid = uuid.Nil.
func uuidFromPg(p pgtype.UUID) uuid.UUID {
	if !p.Valid {
		return uuid.Nil
	}
	return uuid.UUID(p.Bytes)
}

// timeFromPg unwraps a pgtype.Timestamptz into time.Time (zero on NULL).
func timeFromPg(p pgtype.Timestamptz) time.Time {
	if !p.Valid {
		return time.Time{}
	}
	return p.Time.UTC()
}

// timeFromPgDate unwraps a pgtype.Date into a midnight-UTC time.Time.
func timeFromPgDate(p pgtype.Date) time.Time {
	if !p.Valid {
		return time.Time{}
	}
	return p.Time.UTC()
}

// stringPtrFromValue converts an empty string to a nil *string, else
// returns a pointer to the trimmed value. Used for optional varchar
// columns where empty + NULL share semantic.
func stringPtrFromValue(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
