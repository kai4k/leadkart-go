// User management = per-tenant Membership lifecycle.
//
// Naming note: in LeadKart's HTTP vocabulary "user" maps to a
// [membership.Membership] (per-tenant context), not [person.Person]
// (global identity). The split mirrors Auth0 Organizations + Microsoft
// Entra ID per `multi-tenancy.md` "Identity model".
//
// Per ADR 0062 every command here carries the TenantID explicitly —
// the HTTP layer projects it from the JWT before constructing the
// command. Repository methods bind it onto the SQL adapter's
// app.tenant_id GUC for RLS; the fake filters by it.

package command

import (
	"context"
	"errors"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Sentinel ------------------------------------------------------------

// ErrUserNotFound surfaces when the membership ID has no row in the
// caller's tenant — collapses "wrong tenant" + "doesn't exist" into
// one error to defeat membership-id enumeration across tenants.
var ErrUserNotFound = errors.New("user: not found")

// ----- UpdateUserProfile ---------------------------------------------------

// UpdateUserProfileCommand updates the per-Membership profile fields
// (designation, department, free-text status message). Person-level
// fields (FirstName, LastName) move via a separate endpoint per the
// `messaging.md` Person/Membership event split.
type UpdateUserProfileCommand struct {
	TenantID      tenant.ID
	MembershipID  membership.ID
	Designation   string
	Department    string
	StatusMessage string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// UpdateUserProfileHandler runs the profile update.
type UpdateUserProfileHandler struct{ w authorityWriter }

// NewUpdateUserProfileHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewUpdateUserProfileHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) UpdateUserProfileHandler {
	return UpdateUserProfileHandler{w: newAuthorityWriter("NewUpdateUserProfileHandler", uow, lock, m, r, p, now)}
}

// Handle dispatches to [Membership.UpdateProfile] under the tenant lock. A
// member editing their OWN profile is unchanged; editing ANOTHER member's
// profile is an authority decision ([authority.Actor.MayActOn]: the caller must
// strictly outrank them, owners may act on owners), so identity.users.update
// alone no longer lets an HR manager rewrite an owner's profile. No stamp is
// rotated: a profile edit changes no authority.
func (h UpdateUserProfileHandler) Handle(ctx context.Context, cmd UpdateUserProfileCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_user_profile: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("update_user_profile: membership id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	now := h.w.now()
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, cmd.Actor, now, func(_ context.Context, actor authority.Actor, m *membership.Membership, target authority.Member) error {
		if target.ID() != actor.MembershipID() {
			if err := actor.MayActOn(target); err != nil {
				return err
			}
		}
		return m.UpdateProfile(cmd.Designation, cmd.Department, cmd.StatusMessage, now)
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	return translateAuthorityWrite("update_user_profile", err)
}

// ----- DeactivateUser ------------------------------------------------------

// DeactivateUserCommand transitions an Active Membership to Inactive.
// Reason MUST be non-empty per `data-retention.md` audit canon.
//
// DB-enforced single-Active-Membership invariant: deactivating frees
// the Person to be Active in another tenant (sequential job change).
type DeactivateUserCommand struct {
	TenantID     tenant.ID
	MembershipID membership.ID
	Reason       string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// DeactivateUserHandler runs the deactivate flow.
type DeactivateUserHandler struct{ w authorityWriter }

// NewDeactivateUserHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewDeactivateUserHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) DeactivateUserHandler {
	return DeactivateUserHandler{w: newAuthorityWriter("NewDeactivateUserHandler", uow, lock, m, r, p, now)}
}

// Handle decides [authority.Actor.MayDeactivate] under the tenant lock (the
// owner set is loaded when the target is an owner, the only case the policy
// reads it), then dispatches to [Membership.Deactivate].
func (h DeactivateUserHandler) Handle(ctx context.Context, cmd DeactivateUserCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("deactivate_user: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("deactivate_user: membership id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	now := h.w.now()
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor, m *membership.Membership, target authority.Member) error {
		var owners []authority.Member
		if target.IsOwner() {
			var err error
			if owners, err = h.w.facts.owners(ctx, cmd.TenantID, now); err != nil {
				return err
			}
		}
		if err := actor.MayDeactivate(target, owners); err != nil {
			return err
		}
		if err := m.Deactivate(cmd.Reason, now); err != nil {
			return err
		}
		return h.w.rotateStampIfChanged(ctx, m, "deactivated", now)
	})
	return translateAuthorityWrite("deactivate_user", err)
}

// ----- ReactivateUser ------------------------------------------------------

// ReactivateUserCommand transitions Inactive → Active.
//
// Caller-side invariant: the underlying Person MUST NOT have an
// Active Membership in another tenant (single-Active-Membership rule
// per `multi-tenancy.md` "Identity model"). Aggregate enforces basic
// state-machine; partial unique index enforces global invariant; this
// handler surfaces the partial-index race as a generic 422 via
// ErrInvalid.
type ReactivateUserCommand struct {
	TenantID     tenant.ID
	MembershipID membership.ID
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// ErrPersonActiveElsewhere surfaces when reactivating a membership would give
// its person a second ACTIVE membership (the uq_memberships_person_active
// invariant: a person is active in at most one company). Wire shape: 409
// person_active_elsewhere.
var ErrPersonActiveElsewhere = errors.New("reactivate_user: the person already has an active membership in another company")

// ReactivateUserHandler runs the reactivate flow.
type ReactivateUserHandler struct{ w authorityWriter }

// NewReactivateUserHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewReactivateUserHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) ReactivateUserHandler {
	return ReactivateUserHandler{w: newAuthorityWriter("NewReactivateUserHandler", uow, lock, m, r, p, now)}
}

// Handle decides [authority.Actor.MayReactivate] (MayActOn, plus the ownership
// gate when the target is an owner) under the tenant lock and dispatches to
// [Membership.Reactivate]. Idempotent — already-Active
// returns nil without an event.
func (h ReactivateUserHandler) Handle(ctx context.Context, cmd ReactivateUserCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("reactivate_user: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("reactivate_user: membership id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	now := h.w.now()
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor, m *membership.Membership, target authority.Member) error {
		if err := actor.MayReactivate(target); err != nil {
			return err
		}
		if err := m.Reactivate(now); err != nil {
			return err
		}
		// The stamp rotation takes the person row (FOR NO KEY UPDATE) before this
		// transaction can commit: the same row a person-level change holds from its
		// membership check to its write (PersonChangeGuard), so a reactivation
		// cannot commit in between. Removing the rotation reopens that race
		// (TestE2E_PersonChangeGuard_ReactivationCannotCommit...).
		return h.w.rotateStampIfChanged(ctx, m, "reactivated", now)
	})
	if errors.Is(err, membership.ErrAlreadyActive) {
		return ErrPersonActiveElsewhere
	}
	return translateAuthorityWrite("reactivate_user", err)
}
