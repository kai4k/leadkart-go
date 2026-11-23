package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
)

// arch-test:idempotency-via-dedup-window — every handler routes through
// CreateNotification → repository.Add, whose time-windowed dedup probe
// suppresses a re-delivered source event (ErrDuplicateInDedupWindow → success);
// the router IdempotencyMiddleware is the primary dedup.

// Handlers returns the Notifications in-module cqrs handlers — all cross-module
// consumers (subscriber-decides fan-out). Each arg may be nil in test fixtures
// that opt out; nils are skipped.
//
// Post-cqrs (ADR 0067): the EventProcessor derives each handler's subscribe
// topic from the event alias (crm.lead_assigned.v1 → crm.events, etc.), so this
// takes no topic strings.
func Handlers(
	leadAssigned *LeadAssignedSubscriber,
	workItemCreated *WorkItemCreatedSubscriber,
	workItemOverdue *WorkItemOverdueSubscriber,
	orderConfirmed *OrderConfirmedSubscriber,
	stockAlerts *StockAlertSubscriber,
) []cqrs.EventHandler {
	var handlers []cqrs.EventHandler
	if leadAssigned != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerLeadAssigned, leadAssigned.Handle))
	}
	if workItemCreated != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerWorkItemCreated, workItemCreated.Handle))
	}
	if workItemOverdue != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerWorkItemOverdue, workItemOverdue.Handle))
	}
	if orderConfirmed != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerOrderConfirmed, orderConfirmed.Handle))
	}
	if stockAlerts != nil {
		handlers = append(handlers, StockAlertHandlers(stockAlerts)...)
	}
	return handlers
}

// StockAlertHandlers registers the three Inventory stock alerts (expiring soon,
// expired on hand, below reorder level).
func StockAlertHandlers(s *StockAlertSubscriber) []cqrs.EventHandler {
	return []cqrs.EventHandler{
		cqrs.NewEventHandler(HandlerBatchExpiringSoon, s.HandleExpiring),
		cqrs.NewEventHandler(HandlerBatchExpiredOnHand, s.HandleExpiredOnHand),
		cqrs.NewEventHandler(HandlerBelowReorder, s.HandleBelowReorder),
	}
}
