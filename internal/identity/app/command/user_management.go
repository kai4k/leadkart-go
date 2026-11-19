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
	"fmt"
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
}

// UpdateUserProfileHandler runs the profile update.
type UpdateUserProfileHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewUpdateUserProfileHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewUpdateUserProfileHandler(m membership.Repository, now func() time.Time) UpdateUserProfileHandler {
	if m == nil {
		panic("command: NewUpdateUserProfileHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateUserProfileHandler{memberships: m, now: now}
}

// Handle dispatches to [Membership.UpdateProfile].
func (h UpdateUserProfileHandler) Handle(ctx context.Context, cmd UpdateUserProfileCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_user_profile: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("update_user_profile: membership id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.TenantID, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.UpdateProfile(cmd.Designation, cmd.Department, cmd.StatusMessage, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("update_user_profile: %w", err)
	}
	return nil
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
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, now, func(ctx context.Context, m *membership.Membership, target authority.Member) error {
		var owners []authority.Member
		if target.IsOwner() {
			var err error
			if owners, err = h.w.facts.owners(ctx, cmd.TenantID, now); err != nil {
				return err
			}
		}
		if err := cmd.Actor.MayDeactivate(target, owners); err != nil {
			return err
		}
		return m.Deactivate(cmd.Reason, now)
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

// ReactivateUserHandler runs the reactivate flow.
type ReactivateUserHandler struct{ w authorityWriter }

// NewReactivateUserHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewReactivateUserHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) ReactivateUserHandler {
	return ReactivateUserHandler{w: newAuthorityWriter("NewReactivateUserHandler", uow, lock, m, r, p, now)}
}

// Handle decides [authority.Actor.MayActOn] under the tenant lock and
// dispatches to [Membership.Reactivate]. Idempotent — already-Active
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
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, now, func(_ context.Context, m *membership.Membership, target authority.Member) error {
		if err := cmd.Actor.MayActOn(target); err != nil {
			return err
		}
		return m.Reactivate(now)
	})
	return translateAuthorityWrite("reactivate_user", err)
}
