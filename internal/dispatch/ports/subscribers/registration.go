package subscribers

import (
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// arch-test:idempotency-via-router-middleware — wire-up file only; the
// messaging.Router this file binds to is constructed in the composition
// root with IdempotencyMiddleware on every subscriber, so dedup
// happens at the router layer before any Handle is called.

// Register wires every Dispatch in-module subscriber against the
// supplied router. Called once at the composition root (cmd/worker —
// Dispatch does not subscribe from the request path).
//
// The OrderPacked subscriber rides the Orders module's `orders.events`
// topic — handler-side `event_type` metadata filtering routes only
// `orders.order_packed.v1` to the create-consignment-note handler.
//
// ordersTopic defaults to "orders.events" when empty. Production wires
// the integrationevents.Topic constant from the Orders module once
// both branches merge; tests pass an override.
func Register(
	router *messaging.Router,
	orderPacked *OrderPackedIngestor,
	ordersTopic string,
	log *slog.Logger,
) {
	if orderPacked == nil {
		return // test fixtures may opt out
	}
	if ordersTopic == "" {
		ordersTopic = "orders.events"
	}
	_ = log // subscriber owns its own logger; param kept for parity
	router.AddSubscriber(HandlerCreateConsignmentNote, ordersTopic, orderPacked.Handle)
}
