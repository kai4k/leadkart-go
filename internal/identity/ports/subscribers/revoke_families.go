package subscribers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// arch-test:idempotency-via-noop-on-replay — Family.Revoke transitions an active family to revoked; calling it on an already-revoked family is a documented no-op (see refreshtoken.Family godoc). Replaying the same event reaches the same terminal state.

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
// subscriber, separate retry policy.
//
// Failure mode: must-succeed. Family revocation is server-side state
// that the rest of the system depends on (refresh-token reuse
// detection, audit, SIEM downstream subscribers). On error this
// handler RETURNS the error so the router's retry middleware re-runs
// it (DefaultRetry: 5 attempts, 200ms→5s exponential) until the DB
// confirms revocation. Idempotent — Family.Revoke is no-op on
// already-revoked families.
//
// Post-cqrs (ADR 0067): topic routing + payload decode move to the
// EventProcessor + WireAliasMarshaler; each handler is the typed
// reaction only — no event_type filter, no json.Unmarshal.
type RevokeFamiliesOnSecurityChange struct {
	families refreshtoken.Repository
	log      *slog.Logger
	now      func() time.Time
}

// NewRevokeFamiliesOnSecurityChange wires the subscriber. Depends on
// the domain interface (Cheney "accept interfaces, return structs") —
// the production wiring point passes *adapters.RefreshTokenFamilyRepository.
//
// `now` is the explicit time source per the clock-injection refactor —
// composition root wires `time.Now`. Nil → time.Now.
func NewRevokeFamiliesOnSecurityChange(
	families refreshtoken.Repository,
	log *slog.Logger,
	now func() time.Time,
) *RevokeFamiliesOnSecurityChange {
	if log == nil {
		panic("subscribers: NewRevokeFamiliesOnSecurityChange log required")
	}
	if now == nil {
		now = time.Now
	}
	return &RevokeFamiliesOnSecurityChange{
		families: families,
		log:      log,
		now:      now,
	}
}

// HandlePasswordChanged handles `identity.person_password_changed.v1`.
func (h *RevokeFamiliesOnSecurityChange) HandlePasswordChanged(
	ctx context.Context, evt *integrationevents.PersonPasswordChangedV1,
) error {
	return h.revokeAll(ctx, evt.PersonID.String(), "password_changed")
}

// HandleAnonymised handles `identity.person_anonymised.v1`.
func (h *RevokeFamiliesOnSecurityChange) HandleAnonymised(
	ctx context.Context, evt *integrationevents.PersonAnonymisedV1,
) error {
	return h.revokeAll(ctx, evt.PersonID.String(), "person_anonymised")
}

// HandleGloballySuspended handles `identity.person_globally_suspended.v1`.
// Compliance/fraud bans MUST kill every refresh-token family for that
// Person across tenants — the global-suspension flag would otherwise
// leave already-issued tokens valid until expiry, defeating the lockout
// purpose. Per `security.md` SecurityStamp rotation triggers.
func (h *RevokeFamiliesOnSecurityChange) HandleGloballySuspended(
	ctx context.Context, evt *integrationevents.PersonGloballySuspendedV1,
) error {
	return h.revokeAll(ctx, evt.PersonID.String(), "globally_suspended")
}

// HandleEmailChanged handles `identity.person_email_changed.v1`. Email IS
// the global identity primary; rotating it without invalidating sessions
// would let stale JWTs continue to authenticate against the new email's
// account. Auth0/Okta canon: every email change forces a re-login.
func (h *RevokeFamiliesOnSecurityChange) HandleEmailChanged(
	ctx context.Context, evt *integrationevents.PersonEmailChangedV1,
) error {
	return h.revokeAll(ctx, evt.PersonID.String(), "email_changed")
}

// HandleMembershipDeactivated is the narrower (PersonID, TenantID)
// revoke — when a tenant Admin deactivates a Membership, only THAT
// tenant's families for that Person should die; the Person may
// continue to operate under another tenant's still-Active Membership.
func (h *RevokeFamiliesOnSecurityChange) HandleMembershipDeactivated(
	ctx context.Context, evt *integrationevents.MembershipDeactivatedV1,
) error {
	return h.revokeForTenant(ctx, evt.PersonID.String(), evt.TenantIDClaim.String(), "membership_deactivated")
}

// tenantRevokePage is how many families one pass of the tenant cascade loads and
// revokes before it asks the repository again; tenantRevokeMaxPages bounds the
// passes (50 000 sessions at the default) so a bug cannot spin the handler.
const (
	tenantRevokePage     = 500
	tenantRevokeMaxPages = 100
)

// HandleTenantSuspended handles `identity.tenant_suspended.v1`: every session of the
// suspended company's members ends (login and refresh already refuse them; this
// kills the families that exist). Access tokens already issued live out their own
// expiry (600 s); that residual is recorded in ADR 0069.
func (h *RevokeFamiliesOnSecurityChange) HandleTenantSuspended(
	ctx context.Context, evt *integrationevents.TenantSuspendedV1,
) error {
	return h.revokeAllForTenant(ctx, evt.TenantID.String(), "tenant_suspended")
}

// HandleTenantMarkedForDeletion handles `identity.tenant_marked_for_deletion.v1`: a
// company in its deletion grace window is not active (owner ruling 2026-11-21), so
// its members' sessions end like a suspension's. Restoring the company needs no
// handler: the families stay revoked and its members sign in again.
func (h *RevokeFamiliesOnSecurityChange) HandleTenantMarkedForDeletion(
	ctx context.Context, evt *integrationevents.TenantMarkedForDeletionV1,
) error {
	return h.revokeAllForTenant(ctx, evt.TenantID.String(), "tenant_not_active")
}

// HandleTenantDeleted handles `identity.tenant_deleted.v1` (the terminal hard
// delete): same cascade. A tenant row that still has families cannot be deleted
// (foreign key), so this is the defence for a future that removes the constraint.
func (h *RevokeFamiliesOnSecurityChange) HandleTenantDeleted(
	ctx context.Context, evt *integrationevents.TenantDeletedV1,
) error {
	return h.revokeAllForTenant(ctx, evt.TenantID.String(), "tenant_deleted")
}

// revokeAllForTenant revokes every active family bound to the tenant, a page at a
// time. Each family is revoked in its own UpdateByID (one family row lock at a time,
// never two: no lock-order edge with the person or membership paths). Revoked
// families leave the active list, so the loop ends when the page is empty. Idempotent
// like every cascade here: a redelivery finds nothing active, and Family.Revoke is a
// no-op on an already revoked family.
func (h *RevokeFamiliesOnSecurityChange) revokeAllForTenant(ctx context.Context, tenantID, reason string) error {
	tid := tenant.ID(tenantID)
	total := 0
	for range tenantRevokeMaxPages {
		page, err := h.families.ListActiveForTenant(ctx, tid, tenantRevokePage)
		if err != nil {
			return fmt.Errorf("subscribers: list families for tenant %s: %w", tenantID, err)
		}
		if len(page) == 0 {
			if total > 0 {
				h.log.InfoContext(ctx, "revoked families on tenant status change",
					"tenant_id", tenantID, "reason", reason, "count", total)
			}
			return nil
		}
		now := h.now()
		for _, f := range page {
			fid := f.ID()
			if err := h.families.UpdateByID(ctx, fid, func(family *refreshtoken.Family) (bool, error) {
				if family.IsRevoked() {
					return false, nil
				}
				if err := family.Revoke(reason, now); err != nil {
					return false, err
				}
				return true, nil
			}); err != nil {
				h.log.ErrorContext(ctx, "revoke family failed",
					"tenant_id", tenantID, "family_id", fid, "reason", reason, "err", err)
				return fmt.Errorf("revoke family %s: %w", fid, err)
			}
			total++
		}
	}
	// More than tenantRevokeMaxPages pages: return an error so the router retries
	// (the next delivery continues with what is left).
	return fmt.Errorf("subscribers: tenant %s still has active families after %d pages", tenantID, tenantRevokeMaxPages)
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
	now := h.now()
	count := 0
	for _, f := range actives {
		if f.TenantID().String() != tenantID {
			continue
		}
		fid := f.ID()
		err := h.families.UpdateByID(ctx, fid, func(family *refreshtoken.Family) (bool, error) {
			if err := family.Revoke(reason, now); err != nil {
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
	now := h.now()
	for _, f := range actives {
		err := h.families.UpdateByID(ctx, f.ID(), func(family *refreshtoken.Family) (bool, error) {
			if err := family.Revoke(reason, now); err != nil {
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
