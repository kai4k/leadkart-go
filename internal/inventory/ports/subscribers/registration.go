package subscribers

import (
	"github.com/ThreeDotsLabs/watermill/components/cqrs"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// arch-test:idempotency-via-router-middleware — the cqrs handlers built here are
// registered via messaging.Router.AddCqrsHandler, which attaches
// IdempotencyMiddleware to every handler; per-handler idempotency strategy is
// documented at each subscriber (reserve = ledger precheck; release =
// outstanding-ledger no-op).

// Handlers returns the Inventory in-module cqrs handlers — cross-module
// consumers of the Orders lifecycle (true-hold reservation, ADR 0063 §4). Each
// arg may be nil in test fixtures that opt out; nils are skipped. The shipment
// of the hold is NOT an event handler: Dispatch's one transaction does it when
// the goods leave (ADR 0072), and `orders.order_packed.v1` no longer touches stock.
//
// Post-cqrs (ADR 0067): the EventProcessor derives each handler's subscribe
// topic from the event alias (orders.order_*.v1 → orders.events), so this takes
// no topic strings. policyChanged is the cross-module consumer of
// companysettings.events that keeps the replica of the company commerce policy
// current (ADR 0070).
func Handlers(
	reserve *ReserveStockSubscriber,
	release *ReleaseStockSubscriber,
	policyChanged *PolicyChangedSubscriber,
) []cqrs.EventHandler {
	var handlers []cqrs.EventHandler
	if reserve != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerReserveStock, reserve.Handle))
	}
	if release != nil {
		// Replay-safe: a release only follows a cancellation, cancellation is terminal, and the
		// release drains only what is still held (nothing held, nothing to do). A late run is
		// therefore always right, and a lost one leaves a cancelled order holding stock for
		// good (no route releases a hold).
		handlers = append(handlers, messaging.ReplaySafe(cqrs.NewEventHandler(HandlerReleaseStock, release.Handle)))
	}
	if policyChanged != nil {
		// Replay-safe: the replica stores a policy only when its version is newer than
		// the stored one, so a late delivery is a no-op or an improvement, never a step back.
		handlers = append(handlers,
			messaging.ReplaySafe(cqrs.NewEventHandler(HandlerPolicyChanged, policyChanged.Handle)),
			messaging.ReplaySafe(cqrs.NewEventHandler(HandlerPolicyRepublished, policyChanged.HandleRepublished)))
	}
	return handlers
}
