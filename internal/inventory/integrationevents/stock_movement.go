package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// StockMovementLoggedV1 — a stock-movement was logged against a batch.
// Consumers: dashboard projector, future low-stock-alert subscriber,
// Orders saga (for Reservation/Release acknowledgements).
//
// Type is the closed-set string ("inbound", "outbound", "adjustment",
// "reservation", "release") matching batch.MovementType.
// Quantity is SIGNED per the StockMovement aggregate's ledger convention
// (negative for Outbound; non-zero for Adjustment).
type StockMovementLoggedV1 struct {
	MovementID        uuid.UUID `json:"movement_id"`
	BatchID           uuid.UUID `json:"batch_id"`
	ProductID         uuid.UUID `json:"product_id"`
	TenantIDClaim     uuid.UUID `json:"tenant_id"`
	Type              string    `json:"type"`
	Quantity          int64     `json:"quantity"`
	NewQuantityOnHand int64     `json:"new_quantity_on_hand"`
	ActorMembershipID uuid.UUID `json:"actor_membership_id"`
	SourceReference   *string   `json:"source_reference,omitempty"`
	OccurredAtUTC     time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (StockMovementLoggedV1) Topic() string { return "inventory.stock_movement_logged.v1" }

// OccurredAt returns the domain timestamp.
func (e StockMovementLoggedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e StockMovementLoggedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time + runtime registration.
var (
	_ TenantScoped = StockMovementLoggedV1{}

	_ = register(StockMovementLoggedV1{})
)
