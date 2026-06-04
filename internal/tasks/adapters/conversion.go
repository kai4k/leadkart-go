// Package adapters holds Tasks-module outbound adapters per ADR 0002:
// pg-backed WorkItem repository + outbox writer. Concrete (non-interface)
// types — domain consumers in internal/tasks/app/ depend on the interface
// declared in internal/tasks/domain/workitem/repository.go.
//
// All pgtype⇄scalar conversions go through internal/common/pgconv per
// ADR 0066 (single-source). The helpers below are the only ones pgconv
// does not cover: a string-form optional UUID adapter + two NULL→"" row
// readers (pgconv returns uuid.Nil / keeps the pointer; the WorkItem
// snapshot wants an empty string).
package adapters

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leadkart/leadkart-go/internal/common/pgconv"
)

// pgUUIDOpt adapts a string-form optional UUID to pgtype.UUID. Empty or
// unparseable input maps to NULL (Valid=false); a valid UUID delegates
// to pgconv.PgUUID. Domain IDs are uuid.Parse-validated upstream (H6),
// so the parse here only ever fails on the intentionally-empty case.
func pgUUIDOpt(s string) pgtype.UUID {
	parsed, err := uuid.Parse(s)
	if err != nil {
		return pgconv.PgUUIDOrNull(uuid.Nil)
	}
	return pgconv.PgUUIDOrNull(parsed)
}

// uuidStringOrEmpty returns the string form of a pgtype.UUID, or "" if
// the column is NULL (Valid=false). pgconv.UUIDFromPg yields uuid.Nil on
// NULL — its String() would be the all-zero UUID, not "" — so the NULL
// branch is handled here.
func uuidStringOrEmpty(p pgtype.UUID) string {
	if !p.Valid {
		return ""
	}
	return uuid.UUID(p.Bytes).String()
}

// strFromPgOpt unwraps a *string (nullable text column) into a plain
// string. Nil → "".
func strFromPgOpt(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
