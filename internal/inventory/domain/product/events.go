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

// DeactivatedEvent fires on [Product.Deactivate] — i.e. when is_active
// transitions to false (the product becomes invisible on order forms
// but stays selectable in admin + historical-order queries). Distinct
// from SoftDeletedEvent (terminal soft-delete). Maps to
// ProductDeactivatedV1.
//
// Per ADR 0061 amendment 1: the earlier conflation of SoftDelete and
// Deactivate into a single DeactivatedEvent was a semantic mismatch —
// downstream consumers (search index, picker UI) treat "deactivated"
// (still visible to admins) and "soft-deleted" (invisible everywhere)
// differently, and a single event lost the distinction.
type DeactivatedEvent struct {
	ProductID ID
	TenantID  tenant.ID
	ActorID   membership.ID
	At        time.Time
}

func (DeactivatedEvent) isProductEvent() {}

// SoftDeletedEvent fires on [Product.SoftDelete] — terminal hide. The
// row is invisible to LIVE reads but kept for foreign-key + audit
// integrity. Distinct from DeactivatedEvent. Maps to
// ProductSoftDeletedV1.
type SoftDeletedEvent struct {
	ProductID ID
	TenantID  tenant.ID
	ActorID   membership.ID
	At        time.Time
}

func (SoftDeletedEvent) isProductEvent() {}
