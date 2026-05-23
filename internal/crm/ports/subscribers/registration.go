package subscribers

import (
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// Register wires every CRM in-module subscriber against the supplied
// router. Called once at composition root (cmd/worker — CRM does NOT
// publish events from the request path).
//
// The lead-purchased subscriber subscribes to the SAME topic the
// Platform module publishes on (`platform.events`) — handler-side
// `event_type` metadata filtering routes the right payload to the
// right handler per the established subscriber pattern.
//
// platformTopic is the publish destination wired in the Platform
// module's outbox forwarder. Production wires it from the Platform
// integrationevents constant once that module ships; v0.2 callers
// pass the literal "platform.events" string.
func Register(
	router *messaging.Router,
	ingest *PurchasedLeadIngestor,
	platformTopic string,
	log *slog.Logger,
) {
	if ingest == nil {
		return // test fixtures may opt out of the subscriber wiring
	}
	if platformTopic == "" {
		platformTopic = "platform.events"
	}
	// log is unused inside Register itself — the subscriber owns its
	// own logger. Param kept for parity with identity-side Register.
	_ = log
	router.AddSubscriber(HandlerIngestLeadPurchased, platformTopic, ingest.Handle)
}
