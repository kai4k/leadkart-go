package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
)

// arch-test:idempotency-via-router-middleware — the cqrs handlers built here are
// registered via messaging.Router.AddCqrsHandler, which attaches
// IdempotencyMiddleware to every handler; per-handler idempotency strategy is
// documented at each subscriber.

// Handlers returns the Orders in-module cqrs handlers. Each arg may be nil in
// test fixtures that opt out — nils are skipped.
//
// Post-cqrs (ADR 0067): the EventProcessor derives each handler's subscribe
// topic from the event alias (orders.order_packed.v1 → orders.events;
// dispatch.consignment_note_created.v1 → dispatch.events; etc.), so this takes
// no topic strings. autoInvoice consumes the module's own OrderPackedV1; the
// two consignment subscribers are cross-module consumers of dispatch.events.
func Handlers(
	autoInvoice *AutoInvoiceSubscriber,
	consignmentCreated *ConsignmentCreatedSubscriber,
	consignmentDelivered *ConsignmentDeliveredSubscriber,
	cancelCompensation *CancelCompensationSubscriber,
) []cqrs.EventHandler {
	var handlers []cqrs.EventHandler
	if autoInvoice != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerAutoInvoice, autoInvoice.Handle))
	}
	if consignmentCreated != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerConsignmentCreated, consignmentCreated.Handle))
	}
	if consignmentDelivered != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerConsignmentDelivered, consignmentDelivered.Handle))
	}
	if cancelCompensation != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerCancelCompensation, cancelCompensation.Handle))
	}
	return handlers
}
