package product

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the sealed marker interface for product domain events.
// Sealed via the unexported isProductEvent(); only types in this package satisfy it.
// Wire concerns (topic, V1 alias) live in internal/inventory/integrationevents per Vernon IDDD ch.8.
type Event interface {
	isProductEvent()
}

// CreatedEvent fires on [New]. ActorID is the initiating membership; set at construction
// so the integration mapper has a single authoritative source.
type CreatedEvent struct {
	ProductID    ID
	TenantID     tenant.ID
	ActorID      membership.ID
	SKU          string
	Name         string
	DosageForm   string
	PackSize     string
	HSNCode      string
	GSTRateBps   int
	Manufacturer string
	At           time.Time
}

func (CreatedEvent) isProductEvent() {}

// UpdatedEvent fires when [Product.Update] mutates one or more fields.
// ChangedFields holds snake_case wire-stable identifiers, sorted alphabetically.
type UpdatedEvent struct {
	ProductID     ID
	TenantID      tenant.ID
	ActorID       membership.ID
	ChangedFields []string
	At            time.Time
}

func (UpdatedEvent) isProductEvent() {}

// DeactivatedEvent fires on [Product.Deactivate] when is_active transitions to false.
// Distinct from SoftDeletedEvent — deactivated products remain visible to admins;
// soft-deleted are invisible everywhere. ADR 0061 amendment 1 split these semantics.
// Maps to ProductDeactivatedV1.
type DeactivatedEvent struct {
	ProductID ID
	TenantID  tenant.ID
	ActorID   membership.ID
	At        time.Time
}

func (DeactivatedEvent) isProductEvent() {}

// SoftDeletedEvent fires on [Product.SoftDelete] — terminal hide.
// Row is invisible to LIVE reads but kept for FK + audit integrity.
// Distinct from DeactivatedEvent. Maps to ProductSoftDeletedV1.
type SoftDeletedEvent struct {
	ProductID ID
	TenantID  tenant.ID
	ActorID   membership.ID
	At        time.Time
}

func (SoftDeletedEvent) isProductEvent() {}
