package messaging

import "github.com/ThreeDotsLabs/watermill/components/cqrs"

// replaySafeHandler is a cqrs handler its module has marked safe to deliver late.
type replaySafeHandler struct{ cqrs.EventHandler }

// ReplaySafe marks h as safe for the dead-letter replay job to deliver again,
// possibly hours after the event happened.
//
// Replay is opt-in, default deny. A handler qualifies only when applying its event
// LATE cannot leave state wrong: either the handler checks what it applies against
// current state (a policy replica stores only a newer version), or the effect has no
// consequence once the world has moved on (a fail-safe revoke, a notification of a
// fact that happened). A handler that moves stock, issues a document or otherwise
// acts on an order's lifecycle does NOT qualify: the order may have been cancelled or
// dispatched since, and the late event would then corrupt it. Its dead letter stays in
// common.dead_letter, visible through the dead-letter log and metric, for a person.
//
// Whoever marks a handler states the reason beside the call.
func ReplaySafe(h cqrs.EventHandler) cqrs.EventHandler {
	return replaySafeHandler{EventHandler: h}
}

// ReplaySafeHandlerNames returns the names of the handlers marked with [ReplaySafe].
func ReplaySafeHandlerNames(handlers []cqrs.EventHandler) []string {
	var names []string
	for _, h := range handlers {
		if _, ok := h.(replaySafeHandler); ok {
			names = append(names, h.HandlerName())
		}
	}
	return names
}
