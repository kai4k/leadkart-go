package integrationevents

import (
	"time"
)

// OrderPackedV1 signals that an order has been packed and is ready for dispatch.
// TenantScoped — consumed by the Dispatch module (fulfillment saga, ADR 0063 §4)
// to create a pending ConsignmentNote slot from the snapshot.
//
// All UUID fields are wire-shaped as strings (ADR 0059).
type OrderPackedV1 struct {
	OrderID               string     `json:"order_id"`
	TenantID              string     `json:"tenant_id"`
	BoxCount              int32      `json:"box_count"`
	WeightGrams           int64      `json:"weight_grams"`
	CarrierName           string     `json:"carrier_name"`
	ExpectedDeliveryAtUTC *time.Time `json:"expected_delivery_at_utc,omitzero"`
	PackedAtUTC           time.Time  `json:"packed_at_utc"`
	PackedByMembershipID  string     `json:"packed_by_membership_id"`
}

// TopicOrderPackedV1 is the canonical wire alias for the order-packed event.
// Producer stamps this on the outbox row; Dispatch subscriber filters on it.
const TopicOrderPackedV1 = "orders.order_packed.v1"

// Topic returns the canonical wire alias.
func (OrderPackedV1) Topic() string { return TopicOrderPackedV1 }

// OccurredAt returns the domain timestamp.
func (e OrderPackedV1) OccurredAt() time.Time { return e.PackedAtUTC }

// TenantIDString satisfies [TenantScoped] for envelope-level routing.
func (e OrderPackedV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = OrderPackedV1{}
	_              = register(OrderPackedV1{})
)
