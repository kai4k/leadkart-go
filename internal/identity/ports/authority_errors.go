package ports

import (
	"errors"
	"net/http"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// writeAuthorityError maps ADR 0069 policy errors to the wire; the ONE
// translation every authority-changing route shares. Returns false when err is
// none of them (the caller continues with its own mapping). Messages are fixed
// strings: the policy's own wording names ranks and members and is not for
// clients.
func writeAuthorityError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, authority.ErrSelfTarget):
		writeError(w, http.StatusForbidden, ErrCodeSelfModificationForbidden, "callers cannot change their own roles, permissions, status or manager")
	case errors.Is(err, authority.ErrInsufficientRank):
		writeError(w, http.StatusForbidden, ErrCodeInsufficientRank, "the caller does not outrank the target")
	case errors.Is(err, authority.ErrGrantExceedsAuthority):
		writeError(w, http.StatusForbidden, ErrCodeGrantExceedsAuthority, "the change confers or removes a permission beyond the caller's authority")
	case errors.Is(err, authority.ErrOwnershipAuthorityRequired):
		writeError(w, http.StatusForbidden, ErrCodeOwnershipAuthorityRequired, "changing a tenant's ownership from outside it needs platform.tenants.ownership")
	case errors.Is(err, authority.ErrImpersonationNotAllowed):
		writeError(w, http.StatusForbidden, ErrCodeImpersonationNotAllowed, "not available during an impersonation session")
	case errors.Is(err, authority.ErrNotApprover):
		writeError(w, http.StatusForbidden, ErrCodeNotApprover, "the caller is not an eligible approver for this request")
	case errors.Is(err, authority.ErrNoAuthority):
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "")
	case errors.Is(err, authority.ErrLastOwnerProtected):
		writeError(w, http.StatusConflict, ErrCodeLastOwnerProtected, "the tenant's last active owner cannot be deactivated or demoted")
	case errors.Is(err, authority.ErrOwnerProtected):
		writeError(w, http.StatusConflict, ErrCodeOwnerProtected, "tenant.admin cannot be revoked from an owner")
	case errors.Is(err, command.ErrPersonMembershipsChanging):
		writeError(w, http.StatusConflict, ErrCodePersonMembershipsChanging, "the person's memberships changed while the change was checked; retry")
	case errors.Is(err, command.ErrMemberRolesInconsistent):
		writeError(w, http.StatusConflict, ErrCodeMemberRolesInconsistent, "the member's assigned roles cannot be loaded consistently")
	case errors.Is(err, authority.ErrManagerNotEligible):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeManagerNotEligible, "the manager must be an active member of this tenant, more senior than the member, and the reporting line must not form a cycle")
	case errors.Is(err, authority.ErrRoleLevelBreaksReporting):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleLevelBreaksReporting, "the new level would leave a member not strictly junior to their manager, or a direct report not strictly junior to them")
	case errors.Is(err, authority.ErrRoleParentNotEligible):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleParentNotEligible, "a parent role must be more senior than its child")
	case errors.Is(err, role.ErrSystemDefault):
		writeError(w, http.StatusConflict, ErrCodeRoleSystemDefault, "system-default roles cannot be changed")
	case errors.Is(err, role.ErrDeleted):
		writeError(w, http.StatusConflict, ErrCodeRoleDeleted, "the role is deleted")
	default:
		return false
	}
	return true
}
