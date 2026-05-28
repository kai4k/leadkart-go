package order

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by [Repository.GetByID] when no row exists
// for the supplied (tenantID, id) pair. Map to HTTP 404.
var ErrNotFound = errors.New("order: not found")

// Repository persists [Order] aggregates. Mutation is via
// [Repository.UpdateByID] which runs the supplied mutator inside a UoW
// tx — TDL canonical "load → mutate → persist → outbox" shape.
//
// NO business-verb methods. (No `Confirm`, no `Cancel`, no
// `MarkPacked`.) Mutations go through Order's named methods inside
// the UpdateByID closure. Enforced by
// [TestArch_TDL_RepositoryHasNoBusinessVerbMethods].
type Repository interface {
	// Add persists a brand-new aggregate (state=quotation_approved).
	// Translates 23505 on (tenant_id, id) PK to a wrapped sentinel.
	Add(ctx context.Context, o *Order) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Order, error)

	// UpdateByID runs mutator against a freshly-loaded aggregate.
	// (true, nil) persists state + drains events in the same tx;
	// (false, nil) is a no-op; non-nil err rolls back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID,
		mutator func(*Order) (bool, error)) error
}
