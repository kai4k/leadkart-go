package quotation

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by [Repository.GetByID] when no row exists
// for the supplied (tenantID, id) pair. Map to HTTP 404 at the port.
var ErrNotFound = errors.New("quotation: not found")

// Repository persists [Quotation] aggregates. Mutation is via
// [Repository.UpdateByID] which runs the supplied mutator against a
// fresh-from-DB hydration inside a UoW tx — TDL canonical "load →
// mutate → persist → outbox" shape.
//
// Append-only revisions land via the [Quotation.Revise] mutator + drain
// to the outbox in the same tx. No business-verb methods on the
// repository (no `Approve`, no `Revise`) — that's a TDL canon ban
// enforced by [TestArch_TDL_RepositoryHasNoBusinessVerbMethods].
type Repository interface {
	// Add persists a brand-new aggregate. Returns an error if a row
	// with the same (tenant_id, id) already exists (translates 23505
	// to a wrapped sentinel).
	Add(ctx context.Context, q *Quotation) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Quotation, error)

	// UpdateByID runs mutator against a freshly-loaded aggregate. If
	// mutator returns (true, nil) the resulting state + drained events
	// are persisted in the same tx. (false, nil) is a no-op (no write,
	// no event). Any non-nil error from mutator rolls back the tx.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID,
		mutator func(*Quotation) (bool, error)) error
}
