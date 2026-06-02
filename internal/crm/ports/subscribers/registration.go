package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
)

// arch-test:idempotency-via-router-middleware  — wire-up file only; the cqrs handlers it builds are registered via messaging.Router.AddCqrsHandler, which attaches IdempotencyMiddleware to every handler, so dedup happens at the router layer before any Handle runs.

// Handlers returns the CRM purchased-lead cqrs handler. ingest may be nil
// in test fixtures that opt out — returns an empty slice then.
//
// Post-cqrs (ADR 0067): the lead-purchased event is a Platform event
// (alias platform.lead_purchased.v1); the EventProcessor derives its
// subscribe topic (platform.events) from the alias, so this no longer
// takes a topic string. The composition root registers the handler via
// messaging.Router.AddCqrsHandler.
func Handlers(ingest *PurchasedLeadIngestor) []cqrs.EventHandler {
	if ingest == nil {
		return nil
	}
	return []cqrs.EventHandler{
		cqrs.NewEventHandler(HandlerIngestLeadPurchased, ingest.Handle),
	}
}
