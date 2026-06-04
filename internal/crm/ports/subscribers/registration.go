package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
)

// arch-test:idempotency-via-router-middleware  — wire-up file only; the cqrs handlers it builds are registered via messaging.Router.AddCqrsHandler, which attaches IdempotencyMiddleware to every handler, so dedup happens at the router layer before any Handle runs.

// Handlers returns the CRM in-module cqrs handlers. Each arg may be nil in
// test fixtures that opt out — nils are skipped.
//
// Post-cqrs (ADR 0067): the EventProcessor derives each handler's subscribe
// topic from the event alias (platform.lead_purchased.v1 → platform.events;
// crm.call_logged.v1 → crm.events), so this no longer takes topic strings. The
// composition root registers each handler via messaging.Router.AddCqrsHandler.
func Handlers(ingest *PurchasedLeadIngestor, callback *CallbackReminderCreator) []cqrs.EventHandler {
	var handlers []cqrs.EventHandler
	if ingest != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerIngestLeadPurchased, ingest.Handle))
	}
	if callback != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerCreateCallbackReminder, callback.Handle))
	}
	return handlers
}
