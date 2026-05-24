package batch

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// Event is the SEALED marker interface for batch domain events.
type Event interface {
	isBatchEvent()
}

// AddedEvent fires on [New]. The wire counterpart is BatchAddedV1.
// ActorID is the membership that added the batch.
type AddedEvent struct {
	BatchID        ID
	ProductID      product.ID
	TenantID       tenant.ID
	ActorID        membership.ID
	BatchNumber    string
	ExpiryDate     time.Time
	QuantityOnHand int64
	At             time.Time
}

func (AddedEvent) isBatchEvent() {}
