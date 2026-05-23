package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// BatchAddedV1 — a new Batch was added to the given Product. Consumers:
// expiry-alert subscribers, stock-dashboard projector.
//
// ExpiryDate is the date-only value (UTC midnight) — the wire stays
// time.Time for JSON simplicity; downstream consumers MUST treat as date.
type BatchAddedV1 struct {
	BatchID             uuid.UUID `json:"batch_id"`
	ProductID           uuid.UUID `json:"product_id"`
	TenantIDClaim       uuid.UUID `json:"tenant_id"`
	BatchNumber         string    `json:"batch_number"`
	ExpiryDate          time.Time `json:"expiry_date"`
	QuantityOnHand      int64     `json:"quantity_on_hand"`
	AddedAt             time.Time `json:"added_at"`
	AddedByMembershipID uuid.UUID `json:"added_by_membership_id"`
	OccurredAtUTC       time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (BatchAddedV1) Topic() string { return "inventory.batch_added.v1" }

// OccurredAt returns the domain timestamp.
func (e BatchAddedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e BatchAddedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time + runtime registration.
var (
	_ TenantScoped = BatchAddedV1{}

	_ = register(BatchAddedV1{})
)
