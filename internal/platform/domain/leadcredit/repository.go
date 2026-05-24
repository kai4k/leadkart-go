package leadcredit

import (
	"context"
	"errors"
)

// ErrNotFound is returned by GetByTenant when no LeadCredit row exists.
var ErrNotFound = errors.New("leadcredit: not found")

// ErrConflict surfaces an optimistic-concurrency conflict — UPDATE
// matched 0 rows because the version mismatched the loaded value. The
// command handler retries up to 3 times with a small jitter before
// surfacing HTTP 409. Per ADR 0059.
var ErrConflict = errors.New("leadcredit: optimistic conflict")

// Repository persists LeadCredit aggregates. UpsertWithVersion is the
// canonical mutation primitive — atomic upsert + optimistic version
// check + outbox drain in one tx.
type Repository interface {
	// GetByTenant returns the row or [ErrNotFound].
	GetByTenant(ctx context.Context, tenantID TenantID) (*LeadCredit, error)

	// UpsertWithVersion either INSERTs a brand-new row (when the in-
	// memory aggregate's Version == 0 + no row exists yet) or UPDATEs
	// the row with a `WHERE version = $loaded` predicate. Returns
	// [ErrConflict] when the update matches 0 rows (version drift).
	//
	// Drains the aggregate's PullEvents to the outbox in the SAME tx.
	UpsertWithVersion(ctx context.Context, l *LeadCredit) error
}
