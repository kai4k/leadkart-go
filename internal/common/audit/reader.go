package audit

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Reader returns paginated audit-log entries from
// buildingblocks.audit_log_entry. Defined here (audit package) as the
// CONSUMER-OWNED interface per Cheney "accept interfaces, return
// structs"; concrete pg-backed implementation lives in
// internal/identity/adapters/ where the sqlc-generated db.* package is
// allowed. App-layer query handlers depend on this interface only —
// no pgx, no pgxpool, no sqlc.
//
// All methods MUST be safe for concurrent use by multiple goroutines.
//
// Per audit-checklist.md §12 — the audit log carries no RLS (operator-
// facing forensic table), so all reads execute under platform-scope
// tx. Authorization is the caller's responsibility (HTTP boundary
// enforces tenant-admin / platform-operator).
//
// Cursor semantics — keyset over (occurred_at_utc DESC, id DESC) per
// ADR 0038. First page sentinel: pass (FirstPageBefore, FirstPageBeforeID).
// Subsequent pages: pass the LAST row's (OccurredAtUTC, ID) from the
// previous page.
type Reader interface {
	// ListByTenant returns up to limit entries scoped to tenantID,
	// strictly older than (before, beforeID) in keyset tuple order.
	ListByTenant(ctx context.Context, tenantID uuid.UUID, before time.Time, beforeID uuid.UUID, limit int32) ([]Entry, error)

	// ListByUser returns up to limit entries scoped to userID,
	// strictly older than (before, beforeID) in keyset tuple order.
	ListByUser(ctx context.Context, userID uuid.UUID, before time.Time, beforeID uuid.UUID, limit int32) ([]Entry, error)
}

// FirstPageBefore is the sentinel timestamp passed when no cursor is
// supplied (first page). A future date guarantees the keyset tuple
// admits every existing row.
var FirstPageBefore = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// FirstPageBeforeID is the sentinel UUID paired with [FirstPageBefore].
// uuid.Max — ordered strictly greater than every real v7/v4 UUID by
// byte-comparison, so the (occurred_at_utc, id) < (sentinel, sentinel)
// keyset predicate matches every row.
var FirstPageBeforeID = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
