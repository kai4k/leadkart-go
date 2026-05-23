// Package adapters holds the Platform module's pgx/sqlc-backed
// outbound implementations. Mirrors internal/identity/adapters/ shape.
package adapters

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// pgUUID wraps a uuid.UUID into pgtype.UUID for sqlc query parameters.
// Always Valid — zero UUID becomes Valid=true with all-zero bytes.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// pgTimestamp wraps a time.Time into pgtype.Timestamptz. Zero time
// maps to Valid=false (NULL).
func pgTimestamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// pgRequiredTimestamp wraps a non-nullable timestamp. Always Valid=true.
func pgRequiredTimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// uuidFromPg unwraps a pgtype.UUID. Invalid returns uuid.Nil.
func uuidFromPg(p pgtype.UUID) uuid.UUID {
	if !p.Valid {
		return uuid.Nil
	}
	return uuid.UUID(p.Bytes)
}

// timeFromPg unwraps a pgtype.Timestamptz. Invalid returns time.Time{}.
func timeFromPg(p pgtype.Timestamptz) time.Time {
	if !p.Valid {
		return time.Time{}
	}
	return p.Time.UTC()
}
