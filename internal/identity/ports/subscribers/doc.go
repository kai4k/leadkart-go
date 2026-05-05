// Package subscribers holds Identity's IN-MODULE event subscribers —
// the choreographed reactions that fire when an Identity aggregate
// emits an integration event the same module needs to react to.
//
// Per `architecture.md` "Cross-module extensibility — integration
// events, not handler edits": when Module B reacts to Module A, B
// subscribes to A's integration events. THIS package is the
// degenerate "Identity reacts to itself" form — subscribers live in
// the SAME module as the publisher because the side effects belong
// inside Identity (e.g. revoking refresh-token families on
// password-change is an Identity concern, not Notifications').
//
// Cross-module subscribers (CRM reacts to Identity, Notifications
// reacts to Identity, etc.) ship in their own modules' ports/subscribers.
//
// Per `messaging.md` "Cascading messages > IMessageBus injection" +
// "Idempotency — inbox-side required": every subscriber here is
// idempotent (the action it performs is naturally re-runnable OR
// guarded by domain-aggregate state). The IdempotentReceiver
// middleware in the router provides belt-and-suspenders dedup.
package subscribers
