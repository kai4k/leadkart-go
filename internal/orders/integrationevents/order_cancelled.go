package integrationevents

import "time"

// OrderCancelledV1 signals an Order moved to the terminal `cancelled` state.
// TenantScoped — consumed by the compensation subscribers per ADR 0063 §4:
// PriorState decides which fire (invoiced → Invoice→CreditNote; dispatched →
// Dispatch cancel-consignment; confirmed → Inventory unreserve). Drain-mapped:
// every field is present on the domain CancelledEvent.
type OrderCancelledV1 struct {
	OrderID               string    `json:"order_id"`
	TenantID              string    `json:"tenant_id"`
	PriorState            string    `json:"prior_state"`
	Reason                string    `json:"reason"`
	CancelledAtUTC        time.Time `json:"cancelled_at_utc"`
	CancelledByMembership string    `json:"cancelled_by_membership_id"`
}

// TopicOrderCancelledV1 is the canonical wire alias.
const TopicOrderCancelledV1 = "orders.order_cancelled.v1"

// Topic returns the canonical wire alias.
func (OrderCancelledV1) Topic() string { return TopicOrderCancelledV1 }

// OccurredAt returns the domain timestamp.
func (e OrderCancelledV1) OccurredAt() time.Time { return e.CancelledAtUTC }

// TenantIDString satisfies [TenantScoped].
func (e OrderCancelledV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = OrderCancelledV1{}
	_              = register(OrderCancelledV1{})
)
