package authority

import "github.com/leadkart/leadkart-go/internal/common/errs"

// ErrNoAuthority is a zero, cross-tenant or otherwise unauthorised actor. Sentinels (ADR 0069) are mapped to one wire code each by the HTTP port; wrap with fmt.Errorf("%w: detail", ErrX).
var ErrNoAuthority = errs.New(errs.KindPermissionDenied, "authority", "no authority for this action")

// ErrInsufficientRank is returned when the actor does not outrank the target member or role.
var ErrInsufficientRank = errs.New(errs.KindPermissionDenied, "authority", "insufficient rank")

// ErrGrantExceedsAuthority is returned when the actor would confer or remove a permission it may not.
var ErrGrantExceedsAuthority = errs.New(errs.KindPermissionDenied, "authority", "grant exceeds the caller's authority")

// ErrSelfTarget is returned when the actor targets its own roles, permissions, status or manager.
var ErrSelfTarget = errs.New(errs.KindPermissionDenied, "authority", "callers cannot change their own roles, permissions, status or manager")

// ErrImpersonationNotAllowed is returned when the action is refused during an impersonation session.
var ErrImpersonationNotAllowed = errs.New(errs.KindPermissionDenied, "authority", "not available during an impersonation session")

// ErrNotApprover is returned when the actor is not an eligible approver for the request.
var ErrNotApprover = errs.New(errs.KindPermissionDenied, "authority", "caller is not an eligible approver for this request")

// ErrLastOwnerProtected is returned when the tenant would be left without an active owner.
var ErrLastOwnerProtected = errs.New(errs.KindConflict, "authority", "the tenant's last active owner cannot be deactivated or demoted")

// ErrOwnerProtected is returned when tenant.admin may not be revoked from an owner.
var ErrOwnerProtected = errs.New(errs.KindConflict, "authority", "tenant.admin cannot be revoked from an owner")

// ErrManagerNotEligible is returned when the proposed manager is inactive or not more senior than the subordinate.
var ErrManagerNotEligible = errs.New(errs.KindInvalidInput, "authority", "manager must be an active member more senior than the subordinate")

// ErrRoleParentNotEligible is returned when the proposed parent role is not more senior than its child.
var ErrRoleParentNotEligible = errs.New(errs.KindInvalidInput, "authority", "a parent role must be more senior than its child")
