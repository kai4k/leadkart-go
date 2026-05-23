package stockmovement

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// Event is the SEALED marker interface for stock-movement domain events.
type Event interface {
	isMovementEvent()
}

// LoggedEvent fires on [New]. The integration counterpart is
// StockMovementLoggedV1.
type LoggedEvent struct {
	MovementID          ID
	BatchID             batch.ID
	ProductID           product.ID
	TenantID            tenant.ID
	Type                batch.MovementType
	Quantity            int64
	QuantityOnHandAfter int64
	ActorMembershipID   membership.ID
	SourceReference     *string
	At                  time.Time
}

func (LoggedEvent) isMovementEvent() {}
