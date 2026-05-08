package subscribers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/platform/messaging"
)

// RevokeFamiliesOnSecurityChange is the choreographed reaction to
// Person-level security mutations: any change that rotates the
// SecurityStamp (password change, anonymisation, global suspend,
// email change) MUST revoke every refresh-token family for that
// Person across tenants. Plus the narrower membership-deactivation
// cascade which only kills families bound to that (PersonID,
// TenantID) tuple.
//
// Per `security.md` "SecurityStamp rotation triggers": revoke families
// on password/email/role change + logout-all + admin password reset +
// anonymisation. v0.2 wires the password-change + anonymise +
// globally-suspended + email-changed + membership-deactivated
// reactions; future steps add role-change.
//
// Single responsibility: family revocation only. The companion
// [InvalidateSecurityStampCache] subscriber handles the cache
// invalidation concern independently — same events, separate
// subscriber, separate retry policy. See the `Concurrency contract`
// docstring on that type for why the two concerns are split rather
// than coupled inside one handler.
//
// Failure mode: must-succeed. Family revocation is server-side state
// that the rest of the system depends on (refresh-token reuse
// detection, audit, SIEM downstream subscribers). On error this
// handler RETURNS the error so the router's retry middleware re-runs
// it (DefaultRetry: 5 attempts, 200ms→5s exponential) until the DB
// confirms revocation. Idempotent — Family.Revoke is no-op on
// already-revoked families.
type RevokeFamiliesOnSecurityChange struct {
	families *adapters.RefreshTokenFamilyRepository
	log      *slog.Logger
}

// NewRevokeFamiliesOnSecurityChange wires the subscriber.
func NewRevokeFamiliesOnSecurityChange(
	families *adapters.RefreshTokenFamilyRepository,
	log *slog.Logger,
) *RevokeFamiliesOnSecurityChange {
	if log == nil {
		log = slog.Default()
	}
	return &RevokeFamiliesOnSecurityChange{
		families: families,
		log:      log,
	}
}

// HandlePasswordChanged is the [messaging.SubscriberHandler] for the
// `identity.person_password_changed.v1` topic. Other events on the
// shared topic short-circuit silently.
func (h *RevokeFamiliesOnSecurityChange) HandlePasswordChanged(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonPasswordChangedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonPasswordChangedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	return h.revokeAll(ctx, evt.PersonID.String(), "password_changed")
}

// HandleAnonymised is the handler for `identity.person_anonymised.v1`.
func (h *RevokeFamiliesOnSecurityChange) HandleAnonymised(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonAnonymisedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonAnonymisedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	return h.revokeAll(ctx, evt.PersonID.String(), "person_anonymised")
}

// HandleGloballySuspended is the handler for
// `identity.person_globally_suspended.v1`. Compliance/fraud bans
// MUST kill every refresh-token family for that Person across
// tenants — the global-suspension flag would otherwise leave
// already-issued tokens valid until expiry, defeating the lockout
// purpose. Per `security.md` SecurityStamp rotation triggers.
func (h *RevokeFamiliesOnSecurityChange) HandleGloballySuspended(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonGloballySuspendedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonGloballySuspendedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	return h.revokeAll(ctx, evt.PersonID.String(), "globally_suspended")
}

// HandleEmailChanged is the handler for
// `identity.person_email_changed.v1`. Email IS the global identity
// primary; rotating it without invalidating sessions would let stale
// JWTs continue to authenticate against the new email's account.
// Auth0/Okta canon: every email change forces a re-login.
func (h *RevokeFamiliesOnSecurityChange) HandleEmailChanged(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonEmailChangedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonEmailChangedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	return h.revokeAll(ctx, evt.PersonID.String(), "email_changed")
}

// HandleMembershipDeactivated is the narrower (PersonID, TenantID)
// revoke — when a tenant Admin deactivates a Membership, only THAT
// tenant's families for that Person should die; the Person may
// continue to operate under another tenant's still-Active Membership.
//
// The single-Active-Membership invariant per `multi-tenancy.md`
// guarantees at-most-one Active anyway, but a Person can be Inactive
// in one tenant while still authenticated against an unrevoked
// session — the deactivation cascade closes that gap.
func (h *RevokeFamiliesOnSecurityChange) HandleMembershipDeactivated(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.MembershipDeactivatedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.MembershipDeactivatedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	return h.revokeForTenant(ctx, evt.PersonID.String(), evt.TenantIDClaim.String(), "membership_deactivated")
}

// revokeForTenant lists families for the Person + revokes ONLY those
// bound to the supplied TenantID. Used by the membership-deactivated
// cascade, distinct from the broader [revokeAll] used by Person-level
// security events.
func (h *RevokeFamiliesOnSecurityChange) revokeForTenant(
	ctx context.Context, personID, tenantID, reason string,
) error {
	pid := person.ID(personID)
	actives, err := h.families.ListActiveForPerson(ctx, pid)
	if err != nil {
		return fmt.Errorf("subscribers: list families for %s: %w", personID, err)
	}
	count := 0
	for _, f := range actives {
		if f.TenantID().String() != tenantID {
			continue
		}
		fid := f.ID()
		err := h.families.UpdateByID(ctx, fid, func(family *refreshtoken.Family) (bool, error) {
			if err := family.Revoke(reason); err != nil {
				return false, err
			}
			return true, nil
		})
		if err != nil {
			h.log.ErrorContext(ctx, "revoke family failed",
				"person_id", personID, "tenant_id", tenantID, "family_id", fid, "reason", reason, "err", err)
			return fmt.Errorf("revoke family %s: %w", fid, err)
		}
		count++
	}
	if count > 0 {
		h.log.InfoContext(ctx, "revoked tenant-scoped families on membership deactivation",
			"person_id", personID, "tenant_id", tenantID, "count", count)
	}
	return nil
}

// revokeAll lists every NON-revoked family for the Person + revokes
// each via the UpdateByID UpdateFn pattern. Each revoke emits a
// [refreshtoken.RevokedEvent] which the repo drains to outbox as a
// [integrationevents.RefreshTokenFamilyRevokedV1] — downstream SIEM
// + audit subscribers see the cascade.
//
// Failure mode: any non-nil return triggers Watermill retry until
// every active family is confirmed revoked. Cache invalidation is
// NOT this subscriber's concern — see [InvalidateSecurityStampCache]
// running in parallel against the same events with its own
// (opportunistic, TTL-bound) failure semantics.
//
// Idempotency: Family.Revoke is no-op on already-revoked families,
// so re-delivery of the same event (despite the inbox dedup) + any
// retry are both safe.
func (h *RevokeFamiliesOnSecurityChange) revokeAll(
	ctx context.Context, personID string, reason string,
) error {
	pid := person.ID(personID)
	actives, err := h.families.ListActiveForPerson(ctx, pid)
	if err != nil {
		return fmt.Errorf("subscribers: list families for %s: %w", personID, err)
	}
	if len(actives) == 0 {
		return nil
	}
	for _, f := range actives {
		err := h.families.UpdateByID(ctx, f.ID(), func(family *refreshtoken.Family) (bool, error) {
			if err := family.Revoke(reason); err != nil {
				return false, err
			}
			return true, nil
		})
		if err != nil {
			h.log.ErrorContext(ctx, "revoke family failed",
				"person_id", personID, "family_id", f.ID(), "reason", reason, "err", err)
			return fmt.Errorf("revoke family %s: %w", f.ID(), err)
		}
	}
	h.log.InfoContext(ctx, "revoked families on security change",
		"person_id", personID, "reason", reason, "count", len(actives))
	return nil
}
