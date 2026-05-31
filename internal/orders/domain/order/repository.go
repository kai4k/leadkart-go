package order

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by [Repository.GetByID] when no row exists for the
// (tenantID, id) pair. Map to HTTP 404.
var ErrNotFound = errors.New("order: not found")

// Repository persists [Order] aggregates. Mutation goes through
// [Repository.UpdateByID], which runs the mutator inside a UoW tx — the TDL
// canonical "load → mutate → persist → outbox" shape.
//
// NO business-verb methods (no Confirm/Cancel/MarkPacked); mutations call
// Order's named methods inside the UpdateByID closure. Enforced by
// [TestArch_TDL_RepositoryHasNoBusinessVerbMethods].
type Repository interface {
	// Add persists a new aggregate (state=quotation_approved). Translates a
	// 23505 on the (tenant_id, id) PK to a wrapped sentinel.
	Add(ctx context.Context, o *Order) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Order, error)

	// UpdateByID runs mutator against a freshly-loaded aggregate: (true, nil)
	// persists state and drains events in the same tx; (false, nil) is a no-op;
	// a non-nil error rolls back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID,
		mutator func(*Order) (bool, error)) error
}
