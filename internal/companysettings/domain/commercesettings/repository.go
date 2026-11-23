package commercesettings

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Repository persists [Settings].
type Repository interface {
	// GetByID returns the tenant's stored settings, or the defaults at
	// version 0 ([Settings.Version] == 0) when the tenant has no row. It never
	// returns "not found": a tenant with no row reads commercepolicy.Initial.
	GetByID(ctx context.Context, tenantID tenant.ID) (*Settings, error)

	// UpdateByID runs mutator under a row lock on the tenant's policy (the
	// row is created at version 0 when absent), inside one transaction.
	// (true, nil) persists the state, writes the history row for each
	// [PolicyChangedEvent] and the outbox rows for every event, all in that
	// transaction; (false, nil) is a no-op; an error rolls back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, mutator func(*Settings) (bool, error)) error
}
