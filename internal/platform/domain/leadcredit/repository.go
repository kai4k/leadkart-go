package leadcredit

import (
	"context"
	"errors"
)

// ErrNotFound is returned by GetByTenant when no LeadCredit row exists.
var ErrNotFound = errors.New("leadcredit: not found")

// ErrConflict is an optimistic-concurrency conflict: UPDATE matched 0 rows
// because version drifted from the loaded value. The command handler retries up
// to 3 times with jitter before surfacing HTTP 409 (ADR 0059).
var ErrConflict = errors.New("leadcredit: optimistic conflict")

// Repository persists LeadCredit aggregates. UpsertWithVersion is the canonical
// mutation primitive: atomic upsert + optimistic version check + outbox drain in
// one tx.
type Repository interface {
	// GetByTenant returns the row or [ErrNotFound].
	GetByTenant(ctx context.Context, tenantID TenantID) (*LeadCredit, error)

	// UpsertWithVersion INSERTs (Version == 0, no existing row) or UPDATEs under
	// `WHERE version = $loaded`, returning [ErrConflict] on 0 rows (version drift).
	// Drains PullEvents to the outbox in the same tx.
	UpsertWithVersion(ctx context.Context, l *LeadCredit) error
}
