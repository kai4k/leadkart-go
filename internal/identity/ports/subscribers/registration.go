package subscribers

import (
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/platform/messaging"
)

// Identity subscriber-handler names (CI-stable). Changing one of these
// makes every previously-processed message "fresh" against
// identity.processed_messages and re-runs the handler on every
// undelivered envelope. Only rename if you intend that behaviour
// (e.g. fixing a serious bug retroactively).
const (
	HandlerRevokeOnPasswordChange       = "identity.subscribers.RevokeFamiliesOnPasswordChange"
	HandlerRevokeOnAnonymise            = "identity.subscribers.RevokeFamiliesOnAnonymise"
	HandlerRevokeOnGloballySuspended    = "identity.subscribers.RevokeFamiliesOnGloballySuspended"
	HandlerRevokeOnEmailChanged         = "identity.subscribers.RevokeFamiliesOnEmailChanged"
	HandlerRevokeOnMembershipDeactivate = "identity.subscribers.RevokeFamiliesOnMembershipDeactivate"
	HandlerReuseDetectedSIEM            = "identity.subscribers.ReuseDetectedSIEM"
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
func Register(
	router *messaging.Router,
	families *adapters.RefreshTokenFamilyRepository,
	log *slog.Logger,
) {
	revoke := NewRevokeFamiliesOnSecurityChange(families, log)
	siem := NewReuseDetectedSIEM(log)

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
	router.AddSubscriber(
		HandlerReuseDetectedSIEM,
		integrationevents.Topic,
		siem.Handle,
	)
}
