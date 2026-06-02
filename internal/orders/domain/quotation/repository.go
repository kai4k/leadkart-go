package quotation

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by [Repository.GetByID] when no row exists
// for the supplied (tenantID, id) pair. Map to HTTP 404 at the port.
var ErrNotFound = errors.New("quotation: not found")

// Repository persists [Quotation] aggregates. Mutation goes through
// [Repository.UpdateByID]: TDL "load → mutate → persist → outbox" run
// inside one UoW tx. No business-verb methods (no Approve/Revise) —
// enforced by [TestArch_TDL_RepositoryHasNoBusinessVerbMethods].
type Repository interface {
	// Add persists a brand-new aggregate. Errors if (tenant_id, id)
	// already exists (23505 translated to a wrapped sentinel).
	Add(ctx context.Context, q *Quotation) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Quotation, error)

	// UpdateByID runs mutator against a freshly-loaded aggregate.
	// (true, nil) persists the new state + drained events in the same
	// tx; (false, nil) is a no-op; non-nil error rolls back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID,
		mutator func(*Quotation) (bool, error)) error
}
