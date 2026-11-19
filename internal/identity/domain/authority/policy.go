package authority

import (
	"fmt"
	"slices"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// NoLevelChange is MayEditRole's "level unchanged" sentinel (matches
// command.UpdateRoleCommand's -1).
const NoLevelChange = -1

func tenantAdminName() string { return permission.IdentityPermissions.Meta.TenantAdmin }

// operatorExempt: slug-anchored non-impersonation operator holding
// platform.users.manage or platform.roles.manage (ADR 0069 R3).
func (a Actor) operatorExempt() bool {
	if !a.valid || !a.platformOperator {
		return false
	}
	pp := permission.IdentityPermissions.Platform
	return a.hasOwn(pp.UsersManage) || a.hasOwn(pp.RolesManage)
}

// exemptOperatorIn: the operator exemption (R3) is for cross-tenant repair
// only. Inside the operator's own tenant it follows rank like anyone else.
func (a Actor) exemptOperatorIn(tenantID tenant.ID) bool {
	return a.operatorExempt() && !sameTenant(tenantID, a.tenantID)
}

// exemptIn: super users everywhere, exempt operators in other tenants.
func (a Actor) exemptIn(tenantID tenant.ID) bool {
	return a.valid && (a.superUser || a.exemptOperatorIn(tenantID))
}

func (a Actor) outranks(r Rank) bool { return a.valid && a.Rank() < r }

// validLevel: a role level is in [HierarchyLevelMin, HierarchyLevelMax].
func validLevel(n int) bool { return n >= role.HierarchyLevelMin && n <= role.HierarchyLevelMax }

// MayActOn — H2: may a change member t (roles, overrides, status, manager)?
func (a Actor) MayActOn(t Member) error {
	switch {
	case !a.valid || !t.valid:
		return ErrNoAuthority
	case a.superUser:
		return nil
	case t.superAdmin:
		return fmt.Errorf("%w: only a super user may change a super-admin member", ErrInsufficientRank)
	case t.id == a.membershipID:
		return ErrSelfTarget
	case a.exemptOperatorIn(t.tenantID):
		return nil
	case !sameTenant(t.tenantID, a.tenantID):
		return ErrNoAuthority
	case a.Rank() == RankOwner && t.owner:
		return nil
	case a.outranks(t.rank):
		return nil
	}
	return fmt.Errorf("%w: rank %d may not change a member of rank %d", ErrInsufficientRank, a.Rank(), t.rank)
}

// mayChangePermissions — H3: may a confer or remove every name in names inside
// tenantID? Unknown or empty names are refused for everyone, super users
// included. tenant.admin is owner-only (R5); platform-reserved names are super-only.
func (a Actor) mayChangePermissions(tenantID tenant.ID, names []string) error {
	if !a.valid {
		return ErrNoAuthority
	}
	for _, n := range names {
		if !permission.IsKnown(n) {
			return fmt.Errorf("%w: %q is not a known permission", ErrGrantExceedsAuthority, n)
		}
	}
	if a.superUser {
		return nil
	}
	for _, n := range names {
		switch {
		case n == tenantAdminName():
			if a.exemptOperatorIn(tenantID) || (a.Rank() == RankOwner && a.hasOwn(n)) {
				continue
			}
			return fmt.Errorf("%w: only an owner may confer or remove %s", ErrGrantExceedsAuthority, n)
		case !permission.ImpliedByTenantAdmin(n):
			return fmt.Errorf("%w: %q is platform-reserved", ErrGrantExceedsAuthority, n)
		case !a.holds(n, tenantID):
			return fmt.Errorf("%w: caller does not hold %s", ErrGrantExceedsAuthority, n)
		}
	}
	return nil
}

// MayAssignRole — H3: role level strictly below the actor (owners: any
// tenant role), super-admin roles super-only, the role in the target's own
// tenant for every actor, and the role's whole permission set within the
// ceiling. Revocation is symmetric.
func (a Actor) MayAssignRole(t Member, r *role.Role) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if !sameTenant(r.TenantID(), t.tenantID) {
		return ErrNoAuthority
	}
	if r.IsSuperAdmin() && !a.superUser {
		return fmt.Errorf("%w: only a super user may assign or revoke a super-admin role", ErrGrantExceedsAuthority)
	}
	if !a.exemptIn(t.tenantID) && a.Rank() != RankOwner && !a.outranks(levelOf(r)) {
		return fmt.Errorf("%w: role level %d is not below rank %d", ErrInsufficientRank, levelOf(r), a.Rank())
	}
	return a.mayChangePermissions(t.tenantID, PermissionNames(r.Permissions()))
}

// MayRevokeRole is symmetric with MayAssignRole (H3) and, when r is the
// CompanyOwner role, also runs GuardLastOwner against owners (every current
// member holding it, loaded in the same locked transaction).
func (a Actor) MayRevokeRole(t Member, r *role.Role, owners []Member) error {
	if err := a.MayAssignRole(t, r); err != nil {
		return err
	}
	if r.IsCompanyOwner() {
		return GuardLastOwner(t, owners)
	}
	return nil
}

// MayChangeOverrides - H2+H3+H6: may a replace t's permission overrides?
// delta is everything the replace confers or removes (Delta of the granted
// sets union Delta of the revoked sets); revokedAfter is the full revoked set
// the member ends up with. It is MayActOn + the grant ceiling over delta + the
// owner-keeps-tenant.admin guard (which binds every actor, super users too).
func (a Actor) MayChangeOverrides(t Member, delta, revokedAfter []string) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if err := a.mayChangePermissions(t.tenantID, delta); err != nil {
		return err
	}
	return guardOwnerOverrides(t, revokedAfter)
}

// MayDeactivate - H2+H6: may a deactivate t? MayActOn + GuardLastOwner against
// owners (every current member holding the CompanyOwner role).
func (a Actor) MayDeactivate(t Member, owners []Member) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	return GuardLastOwner(t, owners)
}

// MayCreateRole — H3: a new role in tenantID with level and initial perms:
// the level strictly below the actor, the tenant the actor's own unless
// exempt, and perms within the ceiling.
func (a Actor) MayCreateRole(tenantID tenant.ID, level int, perms []string) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if !validLevel(level) {
		return fmt.Errorf("%w: level %d is outside [%d, %d]", ErrInsufficientRank, level, role.HierarchyLevelMin, role.HierarchyLevelMax)
	}
	if !a.exemptIn(tenantID) {
		if !sameTenant(tenantID, a.tenantID) {
			return ErrNoAuthority
		}
		if !a.outranks(Rank(level)) {
			return fmt.Errorf("%w: a new role's level %d must be below rank %d", ErrInsufficientRank, level, a.Rank())
		}
	}
	return a.mayChangePermissions(tenantID, perms)
}

// MayEditRole — H3: actor strictly more senior than the role's current level
// and (if changing) its new level; delta (added ∪ removed permissions) within
// the ceiling. Covers rename, re-level, permission edits, parent and delete.
func (a Actor) MayEditRole(r *role.Role, newLevel int, delta []string) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if newLevel != NoLevelChange && !validLevel(newLevel) {
		return fmt.Errorf("%w: new level %d is outside [%d, %d]", ErrInsufficientRank, newLevel, role.HierarchyLevelMin, role.HierarchyLevelMax)
	}
	if !a.superUser && r.IsSuperAdmin() {
		return fmt.Errorf("%w: only a super user may edit a super-admin role", ErrInsufficientRank)
	}
	if !a.exemptIn(r.TenantID()) {
		if !sameTenant(r.TenantID(), a.tenantID) {
			return ErrNoAuthority
		}
		if !a.outranks(levelOf(r)) {
			return fmt.Errorf("%w: role level %d is not below rank %d", ErrInsufficientRank, levelOf(r), a.Rank())
		}
		if newLevel != NoLevelChange && !a.outranks(Rank(newLevel)) {
			return fmt.Errorf("%w: new level %d is not below rank %d", ErrInsufficientRank, newLevel, a.Rank())
		}
	}
	return a.mayChangePermissions(r.TenantID(), delta)
}

// MayDeleteRole - H3/H6: system-default roles are never deleted (any actor);
// otherwise it is MayEditRole with the role's own permissions as the delta,
// because deleting removes every one of them from every holder.
func (a Actor) MayDeleteRole(r *role.Role) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if r.IsSystemDefault() {
		return fmt.Errorf("%w: %q is a system-default role", role.ErrSystemDefault, r.Name())
	}
	return a.MayEditRole(r, NoLevelChange, PermissionNames(r.Permissions()))
}

// MaySetRoleParent — ADR 0058 edges carry no authority (rank is level-only;
// the approver chain is reports_to) but must agree with the ladder: the
// parent is strictly more senior than the child. parent nil = clear.
func (a Actor) MaySetRoleParent(child, parent *role.Role) error {
	if err := a.MayEditRole(child, NoLevelChange, nil); err != nil {
		return err
	}
	if parent == nil {
		return nil
	}
	if !sameTenant(parent.TenantID(), child.TenantID()) {
		return ErrNoAuthority
	}
	if levelOf(parent) >= levelOf(child) {
		return fmt.Errorf("%w: parent level %d must be below child level %d", ErrRoleParentNotEligible, levelOf(parent), levelOf(child))
	}
	return nil
}

// MayReplaceManager - H4: set or replace sub's manager with newMgr. current is
// sub's present manager exactly as loaded (nil only when sub has none); a nil,
// zero or mismatching current is ErrNoAuthority, never a pass-through. It is
// maySetManager + (when sub has a manager) mayUnlinkManager.
func (a Actor) MayReplaceManager(sub, newMgr Member, current *Member) error {
	if err := a.MayActOn(sub); err != nil {
		return err
	}
	if err := checkCurrentManager(sub, current); err != nil {
		return err
	}
	if err := a.maySetManager(sub, newMgr); err != nil {
		return err
	}
	if current != nil {
		return a.mayUnlinkManager(sub, *current)
	}
	return nil
}

// MayClearManager - H4: remove sub's manager; current as for MayReplaceManager.
// Clearing a manager that does not exist is only MayActOn.
func (a Actor) MayClearManager(sub Member, current *Member) error {
	if err := a.MayActOn(sub); err != nil {
		return err
	}
	if err := checkCurrentManager(sub, current); err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	return a.mayUnlinkManager(sub, *current)
}

// checkCurrentManager: current must be exactly sub's loaded manager.
func checkCurrentManager(sub Member, current *Member) error {
	switch {
	case sub.reportsTo == "" && current == nil:
		return nil
	case current == nil:
		return fmt.Errorf("%w: the member has a manager and it was not supplied", ErrNoAuthority)
	case !current.valid || current.id != sub.reportsTo:
		return fmt.Errorf("%w: the supplied current manager is not the member's manager", ErrNoAuthority)
	}
	return nil
}

// maySetManager - the manager is an active same-tenant member strictly more
// senior than the subordinate (no lateral or upward links); the actor is at
// least as senior as the manager. A manager from another tenant is "not eligible".
func (a Actor) maySetManager(sub, mgr Member) error {
	switch {
	case !mgr.valid:
		return ErrNoAuthority
	case !sameTenant(mgr.tenantID, sub.tenantID):
		return fmt.Errorf("%w: manager belongs to another tenant", ErrManagerNotEligible)
	case !mgr.active:
		return fmt.Errorf("%w: manager is not active", ErrManagerNotEligible)
	case mgr.rank >= sub.rank:
		return fmt.Errorf("%w: manager rank %d must be below member rank %d", ErrManagerNotEligible, mgr.rank, sub.rank)
	case a.exemptIn(sub.tenantID), mgr.id == a.membershipID, mgr.rank >= a.Rank():
		return nil
	}
	return fmt.Errorf("%w: cannot appoint a manager more senior than yourself", ErrInsufficientRank)
}

// mayUnlinkManager - removing or replacing current's authority over sub needs
// the actor at least as senior as current.
func (a Actor) mayUnlinkManager(sub, current Member) error {
	if !current.valid || !sameTenant(current.tenantID, sub.tenantID) {
		return ErrNoAuthority
	}
	if a.exemptIn(sub.tenantID) || current.id == a.membershipID || current.rank >= a.Rank() {
		return nil
	}
	return fmt.Errorf("%w: the current manager is more senior than you", ErrInsufficientRank)
}

// MayApprove — H4: (a) qualified manager, (b) tenant.admin holder, (c) super
// or exempt operator (in another tenant); never impersonation, never self; perm
// must be requestable and held by the approver.
func (a Actor) MayApprove(requester Member, perm string) error {
	return a.mayDecide(requester, perm, true)
}

// MayDeny — H4 eligibility without "holds the permission".
func (a Actor) MayDeny(requester Member) error { return a.mayDecide(requester, "", false) }

func (a Actor) mayDecide(req Member, perm string, approving bool) error {
	switch {
	case !a.valid || !req.valid:
		return ErrNoAuthority
	case a.impersonation:
		return ErrImpersonationNotAllowed
	case req.id == a.membershipID:
		return ErrSelfTarget
	case approving && !permission.IsRequestable(perm):
		return fmt.Errorf("%w: %q is not requestable", ErrGrantExceedsAuthority, perm)
	case a.exemptIn(req.tenantID):
		return nil
	case !sameTenant(req.tenantID, a.tenantID):
		return ErrNoAuthority
	case !a.outranks(req.rank):
		return fmt.Errorf("%w: approver rank %d does not outrank requester rank %d", ErrInsufficientRank, a.Rank(), req.rank)
	case approving && !a.holds(perm, req.tenantID):
		return fmt.Errorf("%w: approver does not hold %s", ErrGrantExceedsAuthority, perm)
	case a.hasOwn(tenantAdminName()):
		return nil
	case req.reportsTo == a.membershipID && a.hasOwn(permission.IdentityPermissions.Roles.Approve):
		return nil
	}
	return ErrNotApprover
}

// GuardLastOwner - H6: refuse removing target's ownership (deactivation,
// CompanyOwner revocation) when no OTHER active owner of the same tenant
// remains. owners is every current member holding the CompanyOwner role,
// loaded in the same locked transaction; it is filtered here (IsOwner and
// IsActive; the zero Member sets neither flag), so an inactive owner, a
// non-owner, an invalid Member or another tenant's owner never counts as
// "another owner". Applies to every actor.
func GuardLastOwner(target Member, owners []Member) error {
	if !target.valid {
		return ErrNoAuthority
	}
	if !target.owner || !target.active {
		return nil
	}
	for _, o := range owners {
		if o.owner && o.active && o.id != target.id && sameTenant(o.tenantID, target.tenantID) {
			return nil
		}
	}
	return ErrLastOwnerProtected
}

// guardOwnerOverrides - H6: an owner's effective set always includes tenant.admin.
func guardOwnerOverrides(target Member, revoked []string) error {
	if !target.valid {
		return ErrNoAuthority
	}
	if target.owner && slices.Contains(revoked, tenantAdminName()) {
		return ErrOwnerProtected
	}
	return nil
}

// Delta is the sorted symmetric difference — what a replace confers or removes.
func Delta(before, after []string) []string {
	set := func(xs []string) map[string]struct{} {
		m := make(map[string]struct{}, len(xs))
		for _, x := range xs {
			m[x] = struct{}{}
		}
		return m
	}
	b, af := set(before), set(after)
	out := []string{}
	for n := range b {
		if _, ok := af[n]; !ok {
			out = append(out, n)
		}
	}
	for n := range af {
		if _, ok := b[n]; !ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// PermissionNames maps permissions to their names (nil entries dropped).
func PermissionNames(ps []*permission.Permission) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p != nil {
			out = append(out, p.Name())
		}
	}
	return out
}
