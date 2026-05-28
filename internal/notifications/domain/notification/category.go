package notification

import "fmt"

// Category tags the source-domain of a notification — used by the
// dedup-key partial unique index + by the inbox UI for icon/colour
// selection.
type Category string

// Closed catalogue. Wire-stable lowercase strings — match the CHECK
// constraint on notifications.notifications.category in the init
// migration. Adding a new category lands in a migration + extends
// this enum simultaneously.
const (
	// CategoryLeadAssigned — a CRM lead landed in the recipient's
	// caseload (CrmLeadAssignedV1 subscriber).
	CategoryLeadAssigned Category = "lead_assigned"

	// CategoryLeadConverted — a CRM lead transitioned to converted
	// (CrmLeadConvertedV1 subscriber). Routed to the lead owner +
	// management chain.
	CategoryLeadConverted Category = "lead_converted"

	// CategoryOrderConfirmed — an order moved to confirmed
	// (OrderConfirmedV1 subscriber). Routed to the order creator.
	CategoryOrderConfirmed Category = "order_confirmed"

	// CategoryOrderDelivered — an order moved to delivered
	// (OrderAdvancedV1 with NewState=delivered subscriber).
	CategoryOrderDelivered Category = "order_delivered"

	// CategoryWorkItemAssigned — a Task was assigned to the recipient.
	CategoryWorkItemAssigned Category = "work_item_assigned"

	// CategoryWorkItemOverdue — a Task assigned to the recipient went
	// overdue, OR a subordinate's task went overdue (the management-
	// chain fan-out routes BOTH to the same category; the message
	// content distinguishes).
	CategoryWorkItemOverdue Category = "work_item_overdue"

	// CategoryStockExpiring — a Batch is within its expiry alert
	// window (BatchExpiringSoonV1 subscriber).
	CategoryStockExpiring Category = "stock_expiring"

	// CategoryStockBelowReorder — a Product is below its reorder level
	// (ProductBelowReorderLevelV1 subscriber).
	CategoryStockBelowReorder Category = "stock_below_reorder"

	// CategoryReminder — CRM reminder due (per BRD §4.6).
	CategoryReminder Category = "reminder"
)

// String returns the wire form.
func (c Category) String() string { return string(c) }

// IsValid reports whether c is in the closed catalogue.
func (c Category) IsValid() bool {
	switch c {
	case CategoryLeadAssigned, CategoryLeadConverted, CategoryOrderConfirmed,
		CategoryOrderDelivered, CategoryWorkItemAssigned, CategoryWorkItemOverdue,
		CategoryStockExpiring, CategoryStockBelowReorder, CategoryReminder:
		return true
	}
	return false
}

// ParseCategory turns an untrusted string into a Category or returns
// [ErrInvalid].
func ParseCategory(raw string) (Category, error) {
	c := Category(raw)
	if !c.IsValid() {
		return "", fmt.Errorf("%w: category %q not in catalogue", ErrInvalid, raw)
	}
	return c, nil
}
