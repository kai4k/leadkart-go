package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
)

// arch-test:idempotency-via-router-middleware  — wire-up file only; the cqrs handlers it builds are registered via messaging.Router.AddCqrsHandler, which attaches IdempotencyMiddleware to every handler, so dedup happens at the router layer before any Handle runs.

// Handlers returns the dispatch order-packed cqrs handler. orderPacked
// may be nil in test fixtures that opt out — returns an empty slice then.
//
// Post-cqrs (ADR 0067): topic routing is derived from the event alias by
// the EventProcessor (orders.* → orders.events), so this no longer takes
// a topic string. The composition root registers each handler via
// messaging.Router.AddCqrsHandler.
func Handlers(orderPacked *OrderPackedIngestor) []cqrs.EventHandler {
	if orderPacked == nil {
		return nil
	}
	return []cqrs.EventHandler{
		cqrs.NewEventHandler(HandlerCreateConsignmentNote, orderPacked.Handle),
	}
}
