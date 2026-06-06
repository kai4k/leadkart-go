package integrationevents

import "time"

// OrderLineItem is the wire shape of one confirmed order line. Carries the
// product + quantity an Inventory stock-reservation subscriber needs; prices
// ride along for downstream display. All ints are paise / basis points.
type OrderLineItem struct {
	ProductID     string `json:"product_id"`
	SKU           string `json:"sku"`
	Quantity      int32  `json:"quantity"`
	UnitSalePaise int64  `json:"unit_sale_paise"`
	GstRateBps    int32  `json:"gst_rate_bps"`
}

// OrderConfirmedV1 signals an Order moved to `confirmed` (token paid, stock to
// be reserved). TenantScoped — consumed by Inventory (reserve stock for the
// confirmed lines) per ADR 0063 §4. Enriched: the confirming command supplies
// the line snapshot the bare AdvancedEvent cannot carry, then publishes this
// via the OutboxEnqueuer inside the order's UoW tx.
type OrderConfirmedV1 struct {
	OrderID                 string          `json:"order_id"`
	TenantID                string          `json:"tenant_id"`
	CustomerLeadID          string          `json:"customer_lead_id"`
	Items                   []OrderLineItem `json:"items"`
	GrandTotalPaise         int64           `json:"grand_total_paise"`
	ConfirmedAtUTC          time.Time       `json:"confirmed_at_utc"`
	ConfirmedByMembershipID string          `json:"confirmed_by_membership_id"`
}

// TopicOrderConfirmedV1 is the canonical wire alias.
const TopicOrderConfirmedV1 = "orders.order_confirmed.v1"

// Topic returns the canonical wire alias.
func (OrderConfirmedV1) Topic() string { return TopicOrderConfirmedV1 }

// OccurredAt returns the domain timestamp.
func (e OrderConfirmedV1) OccurredAt() time.Time { return e.ConfirmedAtUTC }

// TenantIDString satisfies [TenantScoped].
func (e OrderConfirmedV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = OrderConfirmedV1{}
	_              = register(OrderConfirmedV1{})
)
