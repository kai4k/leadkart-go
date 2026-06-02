// Package pgconv is the single source of truth for converting between Go
// values and the pgx/pgtype representations sqlc expects at query
// parameters and scans. Before this package each bounded context carried
// its own copy of these wrappers (identity, platform, inventory, crm all
// had a private conversion.go); the logic was identical and drifted only
// by name (pgUUIDOpt vs pgUUIDOrNull). Centralising it keeps one rule:
// let sqlc emit the NULL-capable type (pgtype.X for uuid/time/date, which
// Go can't express as a nil-able primitive), and do the zero<->NULL
// mapping here, at the adapter boundary, never in domain or app code.
package pgconv

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// PgUUID wraps a uuid.UUID as an always-Valid pgtype.UUID. The zero UUID
// is emitted as Valid=true (all-zero bytes), which Postgres accepts as a
// real uuid value — callers reject zero IDs upstream in domain factories.
func PgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// PgUUIDOrNull wraps an optional uuid.UUID. uuid.Nil maps to Valid=false
// (SQL NULL) — used for nullable FK / actor columns.
func PgUUIDOrNull(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// PgTimestamp wraps an optional time.Time. The zero time maps to
// Valid=false (NULL), matching nullable columns like activated_at /
// suspended_at / left_at. Non-zero times are normalised to UTC.
func PgTimestamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// PgRequiredTimestamp wraps a NOT NULL timestamp; always Valid, normalised
// to UTC. Callers guarantee t is non-zero for required columns.
func PgRequiredTimestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// PgTimestampPtr wraps an optional *time.Time into pgtype.Timestamptz: nil maps
// to NULL (Valid=false), a non-nil value is normalised to UTC. For aggregates
// that model nullable timestamps as pointers (e.g. dispatched_at).
func PgTimestampPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// TimePtrFromPg unwraps a pgtype.Timestamptz into a *time.Time: NULL returns
// nil, a valid value returns a UTC pointer. Inverse of [PgTimestampPtr].
func TimePtrFromPg(p pgtype.Timestamptz) *time.Time {
	if !p.Valid {
		return nil
	}
	u := p.Time.UTC()
	return &u
}

// PgDate wraps an optional date-only value into pgtype.Date (sqlc maps SQL
// `date` columns to pgtype.Date). The zero time maps to NULL; time-of-day
// and zone are dropped — the domain canonicalises to UTC at construction.
func PgDate(t time.Time) pgtype.Date {
	if t.IsZero() {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: t.UTC(), Valid: true}
}

// UUIDFromPg unwraps a pgtype.UUID; NULL returns uuid.Nil. Callers needing
// to distinguish NULL from the zero UUID test p.Valid before calling.
func UUIDFromPg(p pgtype.UUID) uuid.UUID {
	if !p.Valid {
		return uuid.Nil
	}
	return uuid.UUID(p.Bytes)
}

// TimeFromPg unwraps a pgtype.Timestamptz; NULL returns the zero time,
// which domain code reads as "never happened". Valid times come back UTC.
func TimeFromPg(p pgtype.Timestamptz) time.Time {
	if !p.Valid {
		return time.Time{}
	}
	return p.Time.UTC()
}

// TimeFromPgDate unwraps a pgtype.Date into a midnight-UTC time.Time; NULL
// returns the zero time.
func TimeFromPgDate(p pgtype.Date) time.Time {
	if !p.Valid {
		return time.Time{}
	}
	return p.Time.UTC()
}

// ZeroToNil converts a zero value to a nil pointer and any other value to
// a pointer to it — the shape sqlc wants for nullable scalar params when
// empty and NULL share meaning. This is load-bearing for dynamic filters:
// returning &"" for an empty filter would inject `WHERE col = ”`,
// matching zero rows and silently emptying an unfiltered listing.
//
// It is intentionally a conditional helper, not Go 1.26 new(expr): new
// always yields a non-nil pointer, whereas the whole point here is nil on
// the zero value.
func ZeroToNil[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}
