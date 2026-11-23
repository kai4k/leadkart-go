package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
)

// arch-test:idempotency-via-router-middleware — the cqrs handlers built here are
// registered via messaging.Router.AddCqrsHandler, which attaches
// IdempotencyMiddleware to every handler; per-handler idempotency strategy is
// documented at each subscriber (reserve = ledger precheck; release/consume =
// outstanding-ledger no-op).

// Handlers returns the Inventory in-module cqrs handlers — all cross-module
// consumers of the Orders lifecycle (true-hold reservation, ADR 0063 §4). Each
// arg may be nil in test fixtures that opt out; nils are skipped.
//
// Post-cqrs (ADR 0067): the EventProcessor derives each handler's subscribe
// topic from the event alias (orders.order_*.v1 → orders.events), so this takes
// no topic strings. policyChanged is the cross-module consumer of
// companysettings.events that keeps the replica of the company commerce policy
// current (ADR 0070).
func Handlers(
	reserve *ReserveStockSubscriber,
	consume *ConsumeStockSubscriber,
	release *ReleaseStockSubscriber,
	policyChanged *PolicyChangedSubscriber,
) []cqrs.EventHandler {
	var handlers []cqrs.EventHandler
	if reserve != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerReserveStock, reserve.Handle))
	}
	if consume != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerConsumeStock, consume.Handle))
	}
	if release != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerReleaseStock, release.Handle))
	}
	if policyChanged != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerPolicyChanged, policyChanged.Handle))
	}
	return handlers
}
