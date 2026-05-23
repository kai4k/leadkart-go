package product

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for product domain events.
// Sealed via the unexported isProductEvent() method so only types in
// this package can satisfy it (same pattern as tenant.Event +
// role.Event).
//
// Domain events do NOT carry wire concerns (Topic / V1 alias /
// occurred-at-as-method). Integration-event versioning lives in
// `internal/inventory/integrationevents/*V1` per Vernon IDDD ch.8.
type Event interface {
	isProductEvent()
}

// CreatedEvent fires when a Product is created via [New]. ActorID is the
// membership that initiated the create — set at construction time so the
// integration mapper has a single, definitive source.
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
// ChangedFields names the wire-stable field identifiers (snake_case) the
// audit + downstream subscribers consume. Sorted alphabetically.
type UpdatedEvent struct {
	ProductID     ID
	TenantID      tenant.ID
	ActorID       membership.ID
	ChangedFields []string
	At            time.Time
}

func (UpdatedEvent) isProductEvent() {}

// DeactivatedEvent fires on [Product.SoftDelete]. "Deactivated" mirrors
// the BRD vocabulary for the soft-delete state — the row is invisible
// to live reads but kept for foreign-key + audit integrity. The
// integration-event counterpart is ProductDeactivatedV1.
type DeactivatedEvent struct {
	ProductID ID
	TenantID  tenant.ID
	ActorID   membership.ID
	At        time.Time
}

func (DeactivatedEvent) isProductEvent() {}
