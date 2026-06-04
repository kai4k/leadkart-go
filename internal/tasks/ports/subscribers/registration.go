package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
)

// arch-test:idempotency-via-router-middleware — wire-up file only; the cqrs
// handlers it builds are registered via messaging.Router.AddCqrsHandler, which
// attaches IdempotencyMiddleware to every handler, so dedup happens at the
// router layer before any Handle runs.

// Handlers returns the Tasks in-module cqrs handlers. Each arg may be nil in
// test fixtures that opt out — nils are skipped.
//
// Post-cqrs (ADR 0067): the EventProcessor derives each handler's subscribe
// topic from the event alias (crm.call_logged.v1 → crm.events;
// crm.lead_converted.v1 → crm.events), so this no longer takes topic strings.
// Both handlers are cross-module consumers of CRM's `crm.events` topic. The
// composition root registers each handler via messaging.Router.AddCqrsHandler.
func Handlers(callLogged *CallLoggedSubscriber, leadConverted *LeadConvertedSubscriber) []cqrs.EventHandler {
	var handlers []cqrs.EventHandler
	if callLogged != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerCallLogged, callLogged.Handle))
	}
	if leadConverted != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerLeadConverted, leadConverted.Handle))
	}
	return handlers
}
