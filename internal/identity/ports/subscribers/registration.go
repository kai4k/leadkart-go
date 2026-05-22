package subscribers

import (
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// Identity subscriber-handler names (CI-stable). Changing one of these
// makes every previously-processed message "fresh" against
// identity.processed_messages and re-runs the handler on every
// undelivered envelope. Only rename if you intend that behaviour
// (e.g. fixing a serious bug retroactively).
//
// Two parallel subscriber families on the stamp-rotation events:
//
//   InvalidateCacheOn*  — opportunistic SecurityStampCache eviction.
//                         WARN-and-continue on failure (TTL is the
//                         contract). NOT retried.
//   RevokeOn*           — must-succeed refresh-token family revocation.
//                         Returns error on failure → Watermill retries.
//
// Each subscriber has its own dedup key so they're tracked
// independently in identity.processed_messages — A succeeding while
// B retries is a valid intermediate state.
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
)

// Register wires every Identity in-module subscriber against the
// supplied router. Called once at composition root (cmd/api or
// cmd/worker depending on which process hosts the subscriber loop).
//
// Per messaging.md doctrine: subscribers stay self-contained; their
// dependencies (repos, log) are passed in here. The router supplies
// the canonical middleware stack (Recoverer + CorrelationID +
// TenantContext globally, then Idempotency + Audit + Retry per
// handler) — subscribers only worry about the action.
//
// stampCache is threaded into the [InvalidateSecurityStampCache]
// subscriber. The two subscribers (cache-evict + family-revoke) ride
// the same events but carry independent retry/failure policies — see
// each type's docstring. stampCache is required (NewInvalidateSecurityStampCache
// panics otherwise); production wires *adapters.SecurityStampCache,
// tests inject a [SecurityStampInvalidator] fake.
//
// families depends on the domain interface [refreshtoken.Repository]
// (Cheney "accept interfaces, return structs") — production wires
// *adapters.RefreshTokenFamilyRepository.
func Register(
	router *messaging.Router,
	families refreshtoken.Repository,
	stampCache SecurityStampInvalidator,
	log *slog.Logger,
) {
	invalidate := NewInvalidateSecurityStampCache(stampCache, log)
	revoke := NewRevokeFamiliesOnSecurityChange(families, log)
	siem := NewReuseDetectedSIEM(log)

	// Cache-evict subscribers (opportunistic; WARN+continue on failure).
	router.AddSubscriber(
		HandlerInvalidateCacheOnPasswordChanged,
		integrationevents.Topic,
		invalidate.HandlePasswordChanged,
	)
	router.AddSubscriber(
		HandlerInvalidateCacheOnAnonymised,
		integrationevents.Topic,
		invalidate.HandleAnonymised,
	)
	router.AddSubscriber(
		HandlerInvalidateCacheOnGloballySuspended,
		integrationevents.Topic,
		invalidate.HandleGloballySuspended,
	)
	router.AddSubscriber(
		HandlerInvalidateCacheOnEmailChanged,
		integrationevents.Topic,
		invalidate.HandleEmailChanged,
	)

	// Family-revoke subscribers (must-succeed; Watermill retries on error).
	router.AddSubscriber(
		HandlerRevokeOnPasswordChange,
		integrationevents.Topic,
		revoke.HandlePasswordChanged,
	)
	router.AddSubscriber(
		HandlerRevokeOnAnonymise,
		integrationevents.Topic,
		revoke.HandleAnonymised,
	)
	router.AddSubscriber(
		HandlerRevokeOnGloballySuspended,
		integrationevents.Topic,
		revoke.HandleGloballySuspended,
	)
	router.AddSubscriber(
		HandlerRevokeOnEmailChanged,
		integrationevents.Topic,
		revoke.HandleEmailChanged,
	)
	router.AddSubscriber(
		HandlerRevokeOnMembershipDeactivate,
		integrationevents.Topic,
		revoke.HandleMembershipDeactivated,
	)

	// SIEM subscriber.
	router.AddSubscriber(
		HandlerReuseDetectedSIEM,
		integrationevents.Topic,
		siem.Handle,
	)
}
