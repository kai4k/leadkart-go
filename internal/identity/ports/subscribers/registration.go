package subscribers

import (
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"

	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// arch-test:idempotency-via-router-middleware — wire-up file only; the cqrs handlers this file builds are registered via messaging.Router.AddCqrsHandler in the composition root, which attaches IdempotencyMiddleware to every handler, so dedup happens at the router layer before any Handle is called.

// Identity subscriber-handler names (CI-stable). Changing one of these
// makes every previously-processed message "fresh" against
// identity.processed_messages and re-runs the handler on every
// undelivered envelope. Only rename if you intend that behaviour.
//
// Two parallel subscriber families on the stamp-rotation events:
//
//	InvalidateCacheOn*  — opportunistic SecurityStampCache eviction.
//	                      WARN-and-continue on failure (TTL is the
//	                      contract). NOT retried (handler returns nil).
//	RevokeOn*           — must-succeed refresh-token family revocation.
//	                      Returns error on failure → Watermill retries.
//
// Each handler has its own dedup key so they're tracked independently in
// identity.processed_messages — A succeeding while B retries is a valid
// intermediate state. Multiple handlers bind the SAME event type
// (e.g. PersonPasswordChangedV1 → both InvalidateCache + Revoke); the
// cqrs EventProcessor fans the event out to every matching handler.
const (
	// Cache-eviction subscribers (Subscriber A — opportunistic).
	HandlerInvalidateCacheOnPasswordChanged   = "identity.subscribers.InvalidateCacheOnPasswordChanged"
	HandlerInvalidateCacheOnAnonymised        = "identity.subscribers.InvalidateCacheOnAnonymised"
	HandlerInvalidateCacheOnGloballySuspended = "identity.subscribers.InvalidateCacheOnGloballySuspended"
	HandlerInvalidateCacheOnEmailChanged      = "identity.subscribers.InvalidateCacheOnEmailChanged"

	// Family-revocation subscribers (Subscriber B — must-succeed).
	HandlerRevokeOnPasswordChange       = "identity.subscribers.RevokeFamiliesOnPasswordChange"
	HandlerRevokeOnAnonymise            = "identity.subscribers.RevokeFamiliesOnAnonymise"
	HandlerRevokeOnGloballySuspended    = "identity.subscribers.RevokeFamiliesOnGloballySuspended"
	HandlerRevokeOnEmailChanged         = "identity.subscribers.RevokeFamiliesOnEmailChanged"
	HandlerRevokeOnMembershipDeactivate = "identity.subscribers.RevokeFamiliesOnMembershipDeactivate"

	// SIEM subscriber.
	HandlerReuseDetectedSIEM = "identity.subscribers.ReuseDetectedSIEM"

	// Email-dispatch subscribers (ADR 0057 — must-succeed; Watermill
	// retries on transient gateway failure).
	HandlerSendPasswordResetEmail      = "identity.subscribers.SendPasswordResetEmail"
	HandlerSendEmailChangeConfirmation = "identity.subscribers.SendEmailChangeConfirmation"
)

// Handlers builds every Identity in-module cqrs event handler. The
// composition root (cmd/worker) registers each on the shared router via
// messaging.Router.AddCqrsHandler, which supplies the canonical
// resilience stack (PoisonQueue + Idempotency + Audit + Retry +
// Recoverer). Subscribers only worry about the typed reaction.
//
// Post-cqrs (ADR 0067): no router.AddSubscriber, no event_type filters,
// no json.Unmarshal — cqrs.NewEventHandler[T] + the WireAliasMarshaler
// own dispatch + decode. Topic routing is derived from each event's
// alias by the EventProcessor (identity.* → identity.events).
//
// stampCache feeds the [InvalidateSecurityStampCache] handlers; families
// feeds [RevokeFamiliesOnSecurityChange]; both depend on domain
// interfaces (Cheney "accept interfaces, return structs"). emailSender
// may be nil — tests that don't exercise the email path skip those two
// handlers to avoid wiring a Recorder.
func Handlers(
	families refreshtoken.Repository,
	stampCache SecurityStampInvalidator,
	emailSender *EmailSender,
	log *slog.Logger,
	now func() time.Time,
) []cqrs.EventHandler {
	if now == nil {
		now = time.Now
	}
	invalidate := NewInvalidateSecurityStampCache(stampCache, log)
	revoke := NewRevokeFamiliesOnSecurityChange(families, log, now)
	siem := NewReuseDetectedSIEM(log)

	handlers := []cqrs.EventHandler{
		// Cache-evict (opportunistic).
		cqrs.NewEventHandler(HandlerInvalidateCacheOnPasswordChanged, invalidate.HandlePasswordChanged),
		cqrs.NewEventHandler(HandlerInvalidateCacheOnAnonymised, invalidate.HandleAnonymised),
		cqrs.NewEventHandler(HandlerInvalidateCacheOnGloballySuspended, invalidate.HandleGloballySuspended),
		cqrs.NewEventHandler(HandlerInvalidateCacheOnEmailChanged, invalidate.HandleEmailChanged),

		// Family-revoke (must-succeed).
		cqrs.NewEventHandler(HandlerRevokeOnPasswordChange, revoke.HandlePasswordChanged),
		cqrs.NewEventHandler(HandlerRevokeOnAnonymise, revoke.HandleAnonymised),
		cqrs.NewEventHandler(HandlerRevokeOnGloballySuspended, revoke.HandleGloballySuspended),
		cqrs.NewEventHandler(HandlerRevokeOnEmailChanged, revoke.HandleEmailChanged),
		cqrs.NewEventHandler(HandlerRevokeOnMembershipDeactivate, revoke.HandleMembershipDeactivated),

		// SIEM.
		cqrs.NewEventHandler(HandlerReuseDetectedSIEM, siem.Handle),
	}

	// Email-dispatch (ADR 0057). Skipped when emailSender is nil.
	if emailSender != nil {
		handlers = append(handlers,
			cqrs.NewEventHandler(HandlerSendPasswordResetEmail, emailSender.HandlePasswordResetEmail),
			cqrs.NewEventHandler(HandlerSendEmailChangeConfirmation, emailSender.HandleEmailChangeConfirmation),
		)
	}
	return handlers
}

// integrationevents import retained for the package's event types used in
// handler signatures (kept explicit so goimports doesn't drop it when the
// only references are in sibling files).
var _ = integrationevents.Topic
