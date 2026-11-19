package authority

import (
	"fmt"
	"slices"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
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

func (a Actor) exempt() bool         { return a.valid && (a.superUser || a.operatorExempt()) }
func (a Actor) outranks(r Rank) bool { return a.rank < r }

// MayActOn — H2: may a change member t (roles, overrides, status, manager)?
func (a Actor) MayActOn(t Member) error {
	switch {
	case !a.valid:
		return ErrNoAuthority
	case a.superUser:
		return nil
	case t.superAdmin:
		return fmt.Errorf("%w: only a super user may change a super-admin member", ErrInsufficientRank)
	case t.id == a.membershipID:
		return ErrSelfTarget
	case a.operatorExempt():
		return nil
	case t.tenantID != a.tenantID:
		return ErrNoAuthority
	case a.rank == RankOwner && t.rank == RankOwner:
		return nil
	case a.outranks(t.rank):
		return nil
	}
	return fmt.Errorf("%w: rank %d may not change a member of rank %d", ErrInsufficientRank, a.rank, t.rank)
}

// MayChangePermissions — H3: may a confer or remove every name in names?
// tenant.admin is owner-only (R5); platform-reserved names are super-only.
func (a Actor) MayChangePermissions(names []string) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if a.superUser {
		return nil
	}
	for _, n := range names {
		switch {
		case n == tenantAdminName():
			if a.operatorExempt() || (a.rank == RankOwner && a.hasOwn(n)) {
				continue
			}
			return fmt.Errorf("%w: only an owner may confer or remove %s", ErrGrantExceedsAuthority, n)
		case !permission.ImpliedByTenantAdmin(n):
			return fmt.Errorf("%w: %q is platform-reserved or unknown", ErrGrantExceedsAuthority, n)
		case !a.Holds(n):
			return fmt.Errorf("%w: caller does not hold %s", ErrGrantExceedsAuthority, n)
		}
	}
	return nil
}

// MayAssignRole — H3: role level strictly below the actor (owners: any
// tenant role), super-admin roles super-only, and the role's whole permission
// set within the ceiling. Revocation is symmetric.
func (a Actor) MayAssignRole(t Member, r *role.Role) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if r.IsSuperAdmin() && !a.superUser {
		return fmt.Errorf("%w: only a super user may assign or revoke a super-admin role", ErrGrantExceedsAuthority)
	}
	if !a.exempt() {
		if r.TenantID() != t.tenantID {
			return ErrNoAuthority
		}
		if a.rank != RankOwner && !a.outranks(Rank(r.HierarchyLevel())) {
			return fmt.Errorf("%w: role level %d is not below rank %d", ErrInsufficientRank, r.HierarchyLevel(), a.rank)
		}
	}
	return a.MayChangePermissions(PermissionNames(r.Permissions()))
}

// MayRevokeRole is symmetric with MayAssignRole (H3).
func (a Actor) MayRevokeRole(t Member, r *role.Role) error { return a.MayAssignRole(t, r) }

// MayCreateRole — H3: a new role's level must be strictly below the actor.
func (a Actor) MayCreateRole(level int) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if a.exempt() || a.outranks(Rank(level)) {
		return nil
	}
	return fmt.Errorf("%w: a new role's level %d must be below rank %d", ErrInsufficientRank, level, a.rank)
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
	if a.superUser {
		return nil
	}
	if r.IsSuperAdmin() {
		return fmt.Errorf("%w: only a super user may edit a super-admin role", ErrInsufficientRank)
	}
	if !a.operatorExempt() {
		if r.TenantID() != a.tenantID {
			return ErrNoAuthority
		}
		if !a.outranks(Rank(r.HierarchyLevel())) {
			return fmt.Errorf("%w: role level %d is not below rank %d", ErrInsufficientRank, r.HierarchyLevel(), a.rank)
		}
		if newLevel != NoLevelChange && !a.outranks(Rank(newLevel)) {
			return fmt.Errorf("%w: new level %d is not below rank %d", ErrInsufficientRank, newLevel, a.rank)
		}
	}
	return a.MayChangePermissions(delta)
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
	if parent.TenantID() != child.TenantID() {
		return ErrNoAuthority
	}
	if parent.HierarchyLevel() >= child.HierarchyLevel() {
		return fmt.Errorf("%w: parent level %d must be below child level %d", ErrRoleParentNotEligible, parent.HierarchyLevel(), child.HierarchyLevel())
	}
	return nil
}

// MaySetManager — H4: actor outranks the subordinate; the manager is an active
// same-tenant member strictly more senior than the subordinate (no lateral or
// upward links); the actor is at least as senior as the manager.
func (a Actor) MaySetManager(sub, mgr Member) error {
	if err := a.MayActOn(sub); err != nil {
		return err
	}
	switch {
	case mgr.tenantID != sub.tenantID:
		return ErrNoAuthority
	case !mgr.active:
		return fmt.Errorf("%w: manager is not active", ErrManagerNotEligible)
	case mgr.rank >= sub.rank:
		return fmt.Errorf("%w: manager rank %d must be below member rank %d", ErrManagerNotEligible, mgr.rank, sub.rank)
	case a.exempt(), mgr.id == a.membershipID, mgr.rank >= a.rank:
		return nil
	}
	return fmt.Errorf("%w: cannot appoint a manager more senior than yourself", ErrInsufficientRank)
}

// MayUnlinkManager — H4: removing or replacing current's authority over sub
// needs the actor at least as senior as current.
func (a Actor) MayUnlinkManager(sub, current Member) error {
	if err := a.MayActOn(sub); err != nil {
		return err
	}
	if a.exempt() || current.id == a.membershipID || current.rank >= a.rank {
		return nil
	}
	return fmt.Errorf("%w: the current manager is more senior than you", ErrInsufficientRank)
}

// MayApprove — H4: (a) qualified manager, (b) tenant.admin holder, (c) super
// or exempt operator; never impersonation, never self; perm must be requestable
// and held by the approver.
func (a Actor) MayApprove(requester Member, perm string) error {
	return a.mayDecide(requester, perm, true)
}

// MayDeny — H4 eligibility without "holds the permission".
func (a Actor) MayDeny(requester Member) error { return a.mayDecide(requester, "", false) }

func (a Actor) mayDecide(req Member, perm string, approving bool) error {
	switch {
	case !a.valid:
		return ErrNoAuthority
	case a.impersonation:
		return ErrImpersonationNotAllowed
	case req.id == a.membershipID:
		return ErrSelfTarget
	case approving && !permission.IsRequestable(perm):
		return fmt.Errorf("%w: %q is not requestable", ErrGrantExceedsAuthority, perm)
	case a.exempt():
		return nil
	case req.tenantID != a.tenantID:
		return ErrNoAuthority
	case !a.outranks(req.rank):
		return fmt.Errorf("%w: approver rank %d does not outrank requester rank %d", ErrInsufficientRank, a.rank, req.rank)
	case approving && !a.Holds(perm):
		return fmt.Errorf("%w: approver does not hold %s", ErrGrantExceedsAuthority, perm)
	case a.hasOwn(tenantAdminName()):
		return nil
	case req.reportsTo == a.membershipID && a.hasOwn(permission.IdentityPermissions.Roles.Approve):
		return nil
	}
	return ErrNotApprover
}

// GuardLastOwner — H6: refuse removing target's ownership (deactivation,
// CompanyOwner revocation) when no OTHER active owner remains. activeOwners is
// loaded inside the same locked transaction.
func GuardLastOwner(target Member, activeOwners []membership.ID) error {
	if !target.owner || !target.active {
		return nil
	}
	for _, id := range activeOwners {
		if id != target.id {
			return nil
		}
	}
	return ErrLastOwnerProtected
}

// GuardOwnerOverrides — H6: an owner's effective set always includes tenant.admin.
func GuardOwnerOverrides(target Member, revoked []string) error {
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
