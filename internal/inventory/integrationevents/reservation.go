package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// ReservationShortfall is one product line a confirmed order could not fully
// reserve: how many units were wanted versus how many were available across
// all live FEFO batches at reservation time.
type ReservationShortfall struct {
	ProductID         uuid.UUID `json:"product_id"`
	RequestedQuantity int64     `json:"requested_quantity"`
	AvailableQuantity int64     `json:"available_quantity"`
}

// StockReservationFailedV1 — a confirmed order could not be fully stock-reserved
// under the true-hold model (ADR 0063 §4). Reservation is all-or-nothing, so
// NOTHING was reserved; ops must intervene (restock, split the order, or
// refund). Consumed by Notifications (operator alert). TenantScoped.
//
// This is the honest counterpart to oversell prevention: when the platform
// accepts an order it cannot stock, it surfaces the gap loudly rather than
// silently overcommitting inventory.
type StockReservationFailedV1 struct {
	OrderID       uuid.UUID              `json:"order_id"`
	TenantIDClaim uuid.UUID              `json:"tenant_id"`
	Shortfalls    []ReservationShortfall `json:"shortfalls"`
	FailedAtUTC   time.Time              `json:"failed_at_utc"`
}

// Topic returns the canonical wire alias.
func (StockReservationFailedV1) Topic() string { return "inventory.stock_reservation_failed.v1" }

// OccurredAt returns the domain timestamp.
func (e StockReservationFailedV1) OccurredAt() time.Time { return e.FailedAtUTC }

// TenantID satisfies [TenantScoped].
func (e StockReservationFailedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time + runtime registration.
var (
	_ TenantScoped = StockReservationFailedV1{}

	_ = register(StockReservationFailedV1{})
)
