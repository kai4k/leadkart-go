package adapters

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// pgUUID wraps a uuid.UUID into pgtype.UUID for sqlc query parameters.
// Always Valid — zero UUID becomes Valid=true with all-zero bytes,
// which the database accepts as a valid uuid value (callers are
// expected to reject zero IDs upstream in domain factories).
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// pgTimestamp wraps a time.Time into pgtype.Timestamptz. Zero time maps
// to Valid=false (NULL in the column).
func pgTimestamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// pgRequiredTimestamp wraps a non-nullable timestamp. Zero is still
// emitted as Valid=true so columns marked NOT NULL receive a value
// (callers ensure t is non-zero for required timestamps).
func pgRequiredTimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// uuidFromPg unwraps a pgtype.UUID into uuid.UUID. Invalid (NULL) values
// return uuid.Nil — callers checking for NULL should test against
// p.Valid before calling this.
func uuidFromPg(p pgtype.UUID) uuid.UUID {
	if !p.Valid {
		return uuid.Nil
	}
	return uuid.UUID(p.Bytes)
}

// timeFromPg unwraps a pgtype.Timestamptz into time.Time. Invalid (NULL)
// values return time.Time{} (zero).
func timeFromPg(p pgtype.Timestamptz) time.Time {
	if !p.Valid {
		return time.Time{}
	}
	return p.Time.UTC()
}

// stringPtr wraps an empty-or-non-empty string into a *string suitable
// for sqlc nullable text params. Empty maps to nil.
func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
