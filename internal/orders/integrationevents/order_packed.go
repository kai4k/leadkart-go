package integrationevents

import (
	"time"
)

// OrderPackedV1 — an Order has been packed and is ready for dispatch.
// TenantScoped: the owning tenant. Consumed by the Dispatch module
// (fulfillment saga, ADR 0063 §4) to create a pending ConsignmentNote
// slot from the snapshot.
//
// All UUID fields are wire-shaped as STRINGS (Stripe / Auth0 canon +
// ADR 0059 frozen brief — cross-language consumers don't need a uuid
// codec). The field shape is the canonical wire contract: the Dispatch
// subscriber decodes exactly this struct. AmountPaisa-style money fields
// (none here) would be integer minor units, never float.
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

// TopicOrderPackedV1 is the canonical wire alias for the order-packed
// event. Single source of truth: the producer stamps it on the outbox
// row and the Dispatch subscriber filters on this same constant, so the
// two cannot drift (the underscore/hyphen mismatch class the
// producer↔consumer bijection gate prevents).
const TopicOrderPackedV1 = "orders.order_packed.v1"

// Topic returns the canonical wire alias.
func (OrderPackedV1) Topic() string { return TopicOrderPackedV1 }

// OccurredAt returns the domain timestamp.
func (e OrderPackedV1) OccurredAt() time.Time { return e.PackedAtUTC }

// TenantIDString satisfies [TenantScoped] — surfaces the tenant field
// for envelope-level routing per `messaging.md` "Tenant channel". The
// wire field is `tenant_id` (string).
func (e OrderPackedV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = OrderPackedV1{}
	_              = register(OrderPackedV1{})
)
