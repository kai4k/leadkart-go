package subscribers

import "time"

// OrderPackedV1 is a LOCAL MIRROR of the `orders.order_packed.v1`
// wire contract. The Orders module ships the canonical struct in
// internal/orders/integrationevents/ but is being built in parallel.
// Once both branches merge, this file's struct will be REPLACED with
// an import from the orders package; the wire payload (JSON) is
// identical so consumers don't need rewiring.
//
// FIELD-SHAPE CONTRACT: every field type matches the future canonical
// shape VERBATIM. IDs are JSON strings (UUID-shaped) per the project
// convention (mirror of crm/ports/subscribers/lead_purchased_payload.go).
//
// Wire alias: `orders.order_packed.v1`. Tenant-scoped.
type OrderPackedV1 struct {
	OrderID                    string    `json:"order_id"`
	TenantID                   string    `json:"tenant_id"`
	BoxCount                   int32     `json:"box_count"`
	WeightGrams                int64     `json:"weight_grams"`
	CarrierName                string    `json:"carrier_name"`
	ExpectedDeliveryAtUTC      *time.Time `json:"expected_delivery_at_utc,omitempty"`
	PackedAtUTC                time.Time `json:"packed_at_utc"`
	PackedByMembershipID       string    `json:"packed_by_membership_id"`
}

// OrderPackedTopic is the canonical wire-alias the Watermill
// subscriber filters on (`message.Metadata.Get(messaging.HeaderEventType)`).
const OrderPackedTopic = "orders.order_packed.v1"
