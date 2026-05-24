// User management = per-tenant Membership lifecycle.
//
// Naming note: in LeadKart's HTTP vocabulary "user" maps to a
// [membership.Membership] (per-tenant context), not [person.Person]
// (global identity). The split mirrors Auth0 Organizations + Microsoft
// Entra ID per `multi-tenancy.md` "Identity model".
//
// All commands here run under the caller's tenant scope — the
// repository's RLS-aware lookup (set by JWT-bridge middleware →
// pgxpool AfterAcquire) makes "wrong tenant" surface as ErrNotFound,
// matching the Auth0/Okta enumeration-safety rule.

package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
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
	if cmd.MembershipID.IsZero() {
		return errors.New("update_user_profile: membership id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
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
	MembershipID membership.ID
	Reason       string
}

// DeactivateUserHandler runs the deactivate flow.
type DeactivateUserHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewDeactivateUserHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewDeactivateUserHandler(m membership.Repository, now func() time.Time) DeactivateUserHandler {
	if m == nil {
		panic("command: NewDeactivateUserHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return DeactivateUserHandler{memberships: m, now: now}
}

// Handle dispatches to [Membership.Deactivate].
func (h DeactivateUserHandler) Handle(ctx context.Context, cmd DeactivateUserCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("deactivate_user: membership id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.Deactivate(cmd.Reason, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("deactivate_user: %w", err)
	}
	return nil
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
	MembershipID membership.ID
}

// ReactivateUserHandler runs the reactivate flow.
type ReactivateUserHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewReactivateUserHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewReactivateUserHandler(m membership.Repository, now func() time.Time) ReactivateUserHandler {
	if m == nil {
		panic("command: NewReactivateUserHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ReactivateUserHandler{memberships: m, now: now}
}

// Handle dispatches to [Membership.Reactivate]. Idempotent — already-
// Active returns nil without an event.
func (h ReactivateUserHandler) Handle(ctx context.Context, cmd ReactivateUserCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("reactivate_user: membership id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.Reactivate(now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("reactivate_user: %w", err)
	}
	return nil
}
