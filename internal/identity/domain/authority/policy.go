package authority

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// NoLevelChange is MayEditRole's "level unchanged" sentinel (matches
// command.UpdateRoleCommand's -1).
const NoLevelChange = -1

// minLevel is the most senior level a role may be created at or moved to, for
// EVERY actor: level 0 is only ever seeded (the CompanyOwner system role).
const minLevel = role.HierarchyLevelOwner + 1

func tenantAdminName() string { return permission.IdentityPermissions.Meta.TenantAdmin }

// validTenant: the id canonicalises. An exemption is only ever granted for a
// tenant that is a real, canonical id; garbage is not "another tenant".
func validTenant(t tenant.ID) bool {
	_, err := ids.Canonical(string(t))
	return err == nil
}

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
// only, and only for a valid tenant id. Inside the operator's own tenant it
// follows rank like anyone else.
func (a Actor) exemptOperatorIn(tenantID tenant.ID) bool {
	return a.operatorExempt() && validTenant(tenantID) && !sameTenant(tenantID, a.tenantID)
}

// ownershipName is platform.tenants.ownership, the permission that lifts the
// ownership gate for an exempt operator.
func ownershipName() string { return permission.IdentityPermissions.Platform.TenantsOwnership }

// mayChangeOwnershipIn is the ownership gate of the operator exemption (R3,
// amended in hierarchy round 4): the exemption covers day-to-day repair of a
// customer tenant, but who OWNS it, which is assigning or revoking the Company
// Owner role, conferring or removing tenant.admin (by role or by override) and
// deactivating an owner, needs platform.tenants.ownership. It is the one place
// that rule lives; every ownership decision below calls it. It binds only an
// operator acting under the exemption; super users and every actor deciding in
// their own tenant are not touched.
func (a Actor) mayChangeOwnershipIn(tenantID tenant.ID) error {
	if a.superUser || !a.exemptOperatorIn(tenantID) || a.hasOwn(ownershipName()) {
		return nil
	}
	return fmt.Errorf("%w: needs %s", ErrOwnershipAuthorityRequired, ownershipName())
}

// exemptIn: super users and exempt operators in other tenants; never for a
// tenant id that does not canonicalise.
func (a Actor) exemptIn(tenantID tenant.ID) bool {
	return a.valid && validTenant(tenantID) && (a.superUser || a.exemptOperatorIn(tenantID))
}

func (a Actor) outranks(r Rank) bool { return a.valid && a.Rank() < r }

// outranksOrEquals: the actor is at least as senior as r.
func (a Actor) outranksOrEquals(r Rank) bool { return a.valid && a.Rank() <= r }

// validLevel: a created or re-levelled role is in [1, HierarchyLevelMax].
func validLevel(n int) bool { return n >= minLevel && n <= role.HierarchyLevelMax }

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
// tenantID? The tenant must canonicalise; unknown or empty names are refused
// for everyone, super users included. tenant.admin is owner-only (R5);
// platform-reserved names are super-only.
func (a Actor) mayChangePermissions(tenantID tenant.ID, names []string) error {
	if !a.valid || !validTenant(tenantID) {
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
			if a.exemptOperatorIn(tenantID) {
				if err := a.mayChangeOwnershipIn(tenantID); err != nil {
					return err
				}
				continue
			}
			if a.Rank() == RankOwner && a.hasOwn(n) {
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

// mayTouchRole: the rank, tenant and super-admin rules shared by assigning and
// revoking r on t. The role's level is strictly below the actor (owners: any
// tenant role), super-admin roles are super-only, and the role is in the
// target's own tenant for every actor. The permission ceiling is applied by
// the callers.
func (a Actor) mayTouchRole(t Member, r *role.Role) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if !sameTenant(r.TenantID(), t.tenantID) {
		return ErrNoAuthority
	}
	if r.IsCompanyOwner() {
		if err := a.mayChangeOwnershipIn(t.tenantID); err != nil {
			return err
		}
	}
	if r.IsSuperAdmin() && !a.superUser {
		return fmt.Errorf("%w: only a super user may assign or revoke a super-admin role", ErrGrantExceedsAuthority)
	}
	if !a.exemptIn(t.tenantID) && a.Rank() != RankOwner && !a.outranks(levelOf(r)) {
		return fmt.Errorf("%w: role level %d is not below rank %d", ErrInsufficientRank, levelOf(r), a.Rank())
	}
	return nil
}

// MayAssignRole — H3: mayTouchRole, a live role (a deleted role is never
// assigned), and the role's whole permission set within the ceiling.
func (a Actor) MayAssignRole(t Member, r *role.Role) error {
	if err := a.mayTouchRole(t, r); err != nil {
		return err
	}
	if !r.IsActive() {
		return fmt.Errorf("%w: %s", role.ErrInactive, r.Name())
	}
	names := PermissionNames(r.Permissions())
	if err := roleMayContain(r, names); err != nil {
		return err
	}
	if err := a.mayChangePermissions(t.tenantID, names); err != nil {
		return err
	}
	return a.mayConfer(names)
}

// MayRevokeRole is symmetric with MayAssignRole (H3) and, when r is the
// CompanyOwner role, also runs GuardLastOwner against owners (every current
// member holding it, loaded in the same locked transaction). A deleted role is
// revoked on rank alone (no ceiling) so its holders can always be cleaned up.
func (a Actor) MayRevokeRole(t Member, r *role.Role, owners []Member) error {
	if err := a.mayTouchRole(t, r); err != nil {
		return err
	}
	if r.IsActive() {
		if err := a.mayChangePermissions(t.tenantID, PermissionNames(r.Permissions())); err != nil {
			return err
		}
	}
	if r.IsCompanyOwner() {
		return GuardLastOwner(t, owners)
	}
	return nil
}

// Override is one granted permission with its expiry; a zero ExpiresAt is a
// permanent grant.
type Override struct {
	Name      string
	ExpiresAt time.Time
}

// Overrides is a member's permission overrides: the live (unexpired) grants
// with their expiry, and the revoked names.
type Overrides struct {
	Granted []Override
	Revoked []string
}

// MayChangeOverrides — H2+H3+H6: may a replace t's overrides with after? The
// "before" is the live snapshot MemberOf took of t, never a caller-supplied
// value, so a forged or stale before cannot shrink the ceiling set. The set is
// every granted or revoked name that is added or removed, plus every granted
// name whose expiry differs (extending a time-bound grant or making it
// permanent changes it). It is MayActOn + the ceiling over that set + the
// owner-keeps-tenant.admin guard on after.Revoked (which binds every actor,
// super users too).
func (a Actor) MayChangeOverrides(t Member, after Overrides) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if err := a.mayUnrevoke(t, after.Revoked); err != nil {
		return err
	}
	if err := tierRefusal(t.Rank(), "a member", grantedNames(after)); err != nil {
		return err
	}
	delta := slices.Concat(grantDelta(t.overrides.Granted, after.Granted), Delta(t.overrides.Revoked, after.Revoked))
	slices.Sort(delta)
	if err := a.mayChangePermissions(t.tenantID, slices.Compact(delta)); err != nil {
		return err
	}
	if err := a.mayConfer(conferredByOverrides(t.overrides, after)); err != nil {
		return err
	}
	return guardOwnerOverrides(t, after.Revoked)
}

// mayConfer - H3: conferring a permission on someone else is for durable
// holders. An actor who holds a name ONLY through a time-bound grant (the fresh
// read records that in the Actor) may not hand it to anyone through the
// overrides route, a role's permission set, or a role assignment: the receiver
// would keep it after the actor's own grant ended. (An approval is bounded
// instead: [Actor.CapApprovalExpiry] ends it with the approver's grant.)
// Removing a name is not conferring it and is not refused here.
func (a Actor) mayConfer(names []string) error {
	for _, n := range names {
		if until, timed := a.holdUntil[n]; timed {
			return fmt.Errorf("%w: %s is held only until %s; conferring it needs a durable grant", ErrGrantExceedsAuthority, n, until.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// conferredByOverrides is what an overrides change hands the member: every name
// newly granted, granted for longer (a later expiry, or permanent instead of
// timed), or taken off the revoked list.
func conferredByOverrides(before, after Overrides) []string {
	was := make(map[string]time.Time, len(before.Granted))
	for _, g := range before.Granted {
		was[g.Name] = g.ExpiresAt
	}
	var out []string
	for _, g := range after.Granted {
		old, had := was[g.Name]
		if !had || laterThan(g.ExpiresAt, old) {
			out = append(out, g.Name)
		}
	}
	for _, n := range before.Revoked {
		if !slices.Contains(after.Revoked, n) {
			out = append(out, n)
		}
	}
	return out
}

// laterThan: expiry a outlasts expiry b (the zero time is "permanent").
func laterThan(a, b time.Time) bool {
	switch {
	case b.IsZero():
		return false
	case a.IsZero():
		return true
	}
	return a.After(b)
}

// Equal reports whether two override states are the same SET of state: the same
// granted names with the same expiries and the same revoked names, in any order
// and however often a name is listed. It compares both ways (a name present on
// one side only, or an extra name on either, makes them differ), so a change
// that leaves the state equal writes nothing and rotates nothing, and a change
// that does not can never be mistaken for one that does.
func (o Overrides) Equal(other Overrides) bool {
	grants := func(in []Override) map[string]time.Time {
		m := make(map[string]time.Time, len(in))
		for _, g := range in {
			m[g.Name] = g.ExpiresAt
		}
		return m
	}
	revoked := func(in []string) map[string]struct{} {
		m := make(map[string]struct{}, len(in))
		for _, n := range in {
			m[n] = struct{}{}
		}
		return m
	}
	a, b := grants(o.Granted), grants(other.Granted)
	if len(a) != len(b) {
		return false
	}
	for name, exp := range a {
		if other, ok := b[name]; !ok || !exp.Equal(other) {
			return false
		}
	}
	ra, rb := revoked(o.Revoked), revoked(other.Revoked)
	if len(ra) != len(rb) {
		return false
	}
	for n := range ra {
		if _, ok := rb[n]; !ok {
			return false
		}
	}
	return true
}

// Version is an opaque fingerprint of this override state: equal for two states
// exactly when they are the same set ([Overrides.Equal]), whatever the order and
// however often a name is listed. It is the precondition of a replace: the read
// hands it out with the LIVE overrides, the save sends it back, and a save whose
// version no longer matches the live state (a grant expired, an approval landed,
// someone else saved) is refused instead of applied. It is never empty, so an
// absent version can never match, and it carries no permission name.
func (o Overrides) Version() string {
	// One line per entry: a grant is "name"@expiry, a revocation is "name". The
	// name is quoted, so no name can imitate a separator, an expiry or a second
	// entry, and a grant can never read as a revocation.
	entries := make([]string, 0, len(o.Granted)+len(o.Revoked))
	for _, g := range o.Granted {
		// The expiry at the precision the column stores (microseconds), so the same
		// stored row gives the same version however it was loaded.
		exp := "permanent"
		if !g.ExpiresAt.IsZero() {
			exp = strconv.FormatInt(g.ExpiresAt.UnixMicro(), 10)
		}
		entries = append(entries, strconv.Quote(g.Name)+"@"+exp)
	}
	for _, n := range o.Revoked {
		entries = append(entries, strconv.Quote(n))
	}
	slices.Sort(entries)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(entries), "\n")))
	return overridesVersionPrefix + base64.RawURLEncoding.EncodeToString(sum[:18])
}

// overridesVersionPrefix names the fingerprint's format, so a later change of
// the recipe makes every outstanding version a mismatch instead of a collision.
const overridesVersionPrefix = "ov1-"

// mayUnrevoke - H3: an explicit revoke is a deliberate denial, so taking a name
// OFF a member's revoked list (by an overrides change or by approving a
// request) needs seniority, not just holding the name: the actor must be Admin
// tier or above (rank <= RankAdmin; an impersonation session is Admin-equivalent),
// or exempt in the member's tenant. Putting names ON the list is unaffected.
func (a Actor) mayUnrevoke(t Member, afterRevoked []string) error {
	var removed []string
	for _, n := range t.overrides.Revoked {
		if !slices.Contains(afterRevoked, n) {
			removed = append(removed, n)
		}
	}
	if len(removed) == 0 || a.exemptIn(t.tenantID) || a.outranksOrEquals(RankAdmin) {
		return nil
	}
	return fmt.Errorf("%w: only the Admin tier or above may lift a revocation (%s)", ErrGrantExceedsAuthority, strings.Join(removed, ", "))
}

// grantDelta is the names added, removed or re-timed between two grant lists.
// A name listed twice with different expiries counts as changed.
func grantDelta(before, after []Override) []string {
	type expiry struct {
		at       time.Time
		conflict bool
	}
	index := func(xs []Override) map[string]expiry {
		m := make(map[string]expiry, len(xs))
		for _, x := range xs {
			if e, ok := m[x.Name]; ok {
				e.conflict = e.conflict || !e.at.Equal(x.ExpiresAt)
				m[x.Name] = e
				continue
			}
			m[x.Name] = expiry{at: x.ExpiresAt}
		}
		return m
	}
	b, af := index(before), index(after)
	out := []string{}
	for n, be := range b {
		if ae, ok := af[n]; !ok || be.conflict || ae.conflict || !be.at.Equal(ae.at) {
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

// MayDeactivate - H2+H6: may a deactivate t? MayActOn + GuardLastOwner against
// owners (every current member holding the CompanyOwner role).
func (a Actor) MayDeactivate(t Member, owners []Member) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if t.owner {
		if err := a.mayChangeOwnershipIn(t.tenantID); err != nil {
			return err
		}
	}
	return GuardLastOwner(t, owners)
}

// MayReactivate - H2+H6: may a reactivate t? MayActOn, and when t is an owner,
// the ownership gate: bringing an owner back is as much an ownership change as
// deactivating one, so an operator acting from outside the tenant needs
// platform.tenants.ownership for it too.
func (a Actor) MayReactivate(t Member) error {
	if err := a.MayActOn(t); err != nil {
		return err
	}
	if t.owner {
		return a.mayChangeOwnershipIn(t.tenantID)
	}
	return nil
}

// MayCreateRole — H3: a new role in tenantID with level and initial perms.
// The tenant must canonicalise (every actor); the level is in [1, 99] for every
// actor, strictly below the actor, in the actor's own tenant unless exempt;
// perms are within the ceiling. parent is optional: when present it must be in
// the same tenant, live, and strictly more senior than level.
func (a Actor) MayCreateRole(tenantID tenant.ID, level int, perms []string, parent *role.Role) error {
	tid, err := tenant.ParseID(string(tenantID))
	if err != nil || !a.valid {
		return ErrNoAuthority
	}
	if !validLevel(level) {
		return fmt.Errorf("%w: level %d is outside [%d, %d]", ErrInsufficientRank, level, minLevel, role.HierarchyLevelMax)
	}
	if err := roleTierRefusal(Rank(level), perms); err != nil {
		return err
	}
	if !a.exemptIn(tid) {
		if !sameTenant(tid, a.tenantID) {
			return ErrNoAuthority
		}
		if !a.outranks(Rank(level)) {
			return fmt.Errorf("%w: a new role's level %d must be below rank %d", ErrInsufficientRank, level, a.Rank())
		}
	}
	if err := a.mayChangePermissions(tid, perms); err != nil {
		return err
	}
	if err := a.mayConfer(perms); err != nil {
		return err
	}
	if parent == nil {
		return nil
	}
	if !sameTenant(parent.TenantID(), tid) {
		return ErrNoAuthority
	}
	return parentEligible(parent, Rank(level))
}

// parentEligible: a parent role is live and strictly more senior than the child level.
func parentEligible(parent *role.Role, childLevel Rank) error {
	if !parent.IsActive() {
		return fmt.Errorf("%w: parent role %q is deleted", ErrRoleParentNotEligible, parent.Name())
	}
	if levelOf(parent) >= childLevel {
		return fmt.Errorf("%w: parent level %d must be below child level %d", ErrRoleParentNotEligible, levelOf(parent), childLevel)
	}
	return nil
}

// mayMutateRole — the rules every role-mutating decision shares: a valid
// actor, a role, never a system-default role (any actor), super-admin roles
// super-only, a new level in [1, 99], and the actor strictly more senior than
// the role's current level and (when re-levelling) its new level, unless exempt.
func (a Actor) mayMutateRole(r *role.Role, newLevel int) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if r == nil {
		return fmt.Errorf("%w: role required", ErrNoAuthority)
	}
	if r.IsSystemDefault() {
		return fmt.Errorf("%w: %q is a system-default role", role.ErrSystemDefault, r.Name())
	}
	if newLevel != NoLevelChange && !validLevel(newLevel) {
		return fmt.Errorf("%w: new level %d is outside [%d, %d]", ErrInsufficientRank, newLevel, minLevel, role.HierarchyLevelMax)
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
	return nil
}

// RoleHolders is the input of every decision that changes what a role confers
// or where it stands: every member holding the role (any status, as loaded by
// the command's facts loader, once per command) and, for a re-level, the members
// the holders are linked to by reports_to that do not hold the role themselves
// (their managers and their direct reports).
type RoleHolders struct {
	Holders []Member
	Linked  []Member
}

// ChangesLevel reports whether newLevel moves r (NoLevelChange and the current
// level do not). Commands use it to decide whether the holders must be loaded
// for a rename-and-re-level edit; the policy uses the same test.
func ChangesLevel(r *role.Role, newLevel int) bool {
	return r != nil && newLevel != NoLevelChange && newLevel != r.HierarchyLevel()
}

// mayChangeRoleFor - H3: changing what a role confers (grant, revoke, replace
// permissions, delete) or re-levelling it is ACTING ON EVERY MEMBER WHO HOLDS
// IT, ACTIVE OR NOT. A role's level need not match its holders' ranks, so the
// role-level checks alone let a manager confer on, or strip from, peers and
// seniors, and let a member make a time-bound permission permanent for
// themself. The actor must be allowed to act on each holder ([Actor.MayActOn]:
// strictly senior, owners over owners, exempt operators and super users). An
// inactive holder counts like any other (fail closed: reactivation does not
// re-check what their roles now confer; whoever outranks them can still remove
// the role from them). A holder who is the actor is refused ErrSelfTarget,
// except for an owner (rank 0): an owner holds tenant.admin and gains nothing
// by editing a custom role they also hold, and without the exemption a sole
// owner could neither edit it nor drop it. It is the ONE place the rule lives,
// reached through every role decision below, which all take the holders as a
// parameter so no command can leave them out.
func (a Actor) mayChangeRoleFor(h RoleHolders) error {
	for _, m := range h.Holders {
		if !m.valid {
			return ErrNoAuthority
		}
		if m.id == a.membershipID && a.Rank() == RankOwner {
			continue
		}
		if err := a.MayActOn(m); err != nil {
			return err
		}
	}
	return nil
}

// reportingHolds - H3: a re-level may not NEWLY break a reporting line. After r
// moves to newLevel, every line that touches an active holder and was strictly
// ordered before the move (holder junior to their manager; each direct report
// junior to the holder) must still be. A line that is already broken, or whose
// other end is inactive, does not block: ordinary commands (a role assigned or
// revoked) can leave such lines behind, and refusing every re-level because of
// them would lock administration. The holder's new rank is the most senior of
// newLevel and the levels of the other live roles they hold; members who do not
// hold r keep their rank. Applies to every actor (it is data integrity, not
// authority). A manager that was not loaded fails closed.
func reportingHolds(r *role.Role, newLevel int, h RoleHolders) error {
	byID := make(map[membership.ID]Member, len(h.Holders)+len(h.Linked))
	holder := make(map[membership.ID]struct{}, len(h.Holders))
	for _, m := range h.Holders {
		byID[m.id] = m
		holder[m.id] = struct{}{}
	}
	for _, m := range h.Linked {
		byID[m.id] = m
	}
	after := func(m Member) Rank {
		if _, ok := holder[m.id]; ok {
			return m.rankIfRoleMovedTo(r.ID(), Rank(newLevel))
		}
		return m.rank
	}
	// newlyBroken: strictly ordered before (junior below senior), not after.
	newlyBroken := func(junior, senior Member) bool {
		return junior.rank > senior.rank && after(junior) <= after(senior)
	}
	for _, hm := range h.Holders {
		if !hm.valid || !hm.statusActive {
			continue
		}
		if hm.reportsTo != "" {
			mgr, ok := byID[hm.reportsTo]
			if !ok {
				return fmt.Errorf("%w: the manager of a holder was not loaded", ErrNoAuthority)
			}
			if mgr.statusActive && newlyBroken(hm, mgr) {
				return fmt.Errorf("%w: a holder would be rank %d under their manager at rank %d", ErrRoleLevelBreaksReporting, after(hm), after(mgr))
			}
		}
		for _, x := range byID {
			if x.reportsTo == hm.id && x.statusActive && newlyBroken(x, hm) {
				return fmt.Errorf("%w: a holder would be rank %d over a direct report at rank %d", ErrRoleLevelBreaksReporting, after(hm), after(x))
			}
		}
	}
	return nil
}

// MayEditRole — H3: rename and re-level only (newLevel = NoLevelChange keeps
// the level). The actor must be strictly more senior than the role's current
// and new level; a system-default role is refused for every actor
// (role.ErrSystemDefault). A re-level also acts on the holders
// ([Actor.mayChangeRoleFor]) and must keep every reporting line strictly
// ordered (ErrRoleLevelBreaksReporting); a rename needs no holders. Permission
// changes go through MayReplaceRolePermissions, MayGrantRolePermission and
// MayRevokeRolePermission, which compute their own delta.
func (a Actor) MayEditRole(r *role.Role, newLevel int, holders RoleHolders) error {
	if err := a.mayMutateRole(r, newLevel); err != nil {
		return err
	}
	if !ChangesLevel(r, newLevel) {
		return nil
	}
	if err := roleTierRefusal(Rank(newLevel), PermissionNames(r.Permissions())); err != nil {
		return err
	}
	if err := a.mayChangeRoleFor(holders); err != nil {
		return err
	}
	return reportingHolds(r, newLevel, holders)
}

// MayReplaceRolePermissions — H3: mayMutateRole + the ceiling over
// Delta(the role's current permissions, after) + the holders rule.
func (a Actor) MayReplaceRolePermissions(r *role.Role, after []string, holders RoleHolders) error {
	if err := a.mayMutateRole(r, NoLevelChange); err != nil {
		return err
	}
	cur := PermissionNames(r.Permissions())
	if err := roleMayContain(r, after); err != nil {
		return err
	}
	if err := a.mayChangePermissions(r.TenantID(), Delta(cur, after)); err != nil {
		return err
	}
	if err := a.mayChangeRoleFor(holders); err != nil {
		return err
	}
	return a.mayConfer(slices.DeleteFunc(slices.Clone(after), func(n string) bool { return slices.Contains(cur, n) }))
}

// MayGrantRolePermission — H3: adding name to r; a name r already holds changes nothing.
func (a Actor) MayGrantRolePermission(r *role.Role, name string, holders RoleHolders) error {
	if err := a.mayMutateRole(r, NoLevelChange); err != nil {
		return err
	}
	cur := PermissionNames(r.Permissions())
	if err := roleMayContain(r, append(slices.Clone(cur), name)); err != nil {
		return err
	}
	if err := a.mayChangePermissions(r.TenantID(), Delta(cur, append(slices.Clone(cur), name))); err != nil {
		return err
	}
	if err := a.mayChangeRoleFor(holders); err != nil {
		return err
	}
	if slices.Contains(cur, name) {
		return nil
	}
	return a.mayConfer([]string{name})
}

// MayRevokeRolePermission — H3: removing name from r; a name r does not hold changes nothing.
func (a Actor) MayRevokeRolePermission(r *role.Role, name string, holders RoleHolders) error {
	if err := a.mayMutateRole(r, NoLevelChange); err != nil {
		return err
	}
	cur := PermissionNames(r.Permissions())
	if err := a.mayChangePermissions(r.TenantID(), Delta(cur, slices.DeleteFunc(slices.Clone(cur), func(n string) bool { return n == name }))); err != nil {
		return err
	}
	return a.mayChangeRoleFor(holders)
}

// MayDeactivateRole — H3/H6: mayMutateRole + the ceiling over the role's own
// permissions, because deactivating removes every one of them from every holder, + the
// holders rule (answered before the role-in-use rule, so a caller who may not act on the
// holders learns nothing about them). A role is never deleted (ADR 0073); this is exactly
// what deleting one used to ask.
func (a Actor) MayDeactivateRole(r *role.Role, holders RoleHolders) error {
	if err := a.mayMutateRole(r, NoLevelChange); err != nil {
		return err
	}
	if err := a.mayChangePermissions(r.TenantID(), PermissionNames(r.Permissions())); err != nil {
		return err
	}
	return a.mayChangeRoleFor(holders)
}

// MayReactivateRole is the decision for giving an inactive role back to its holders. It is a
// CONFERRAL (every holder gets every permission of the role), so it asks everything removing
// them asks ([Actor.MayDeactivateRole]: authority over the role, the ceiling, every holder)
// AND what every other conferring route asks: the role-tier rule over its permission set and a
// durable hold of each name, so a time-bound holder cannot make a permission permanent for
// others by reactivating a role that carries it (the same rule MayAssignRole enforces).
func (a Actor) MayReactivateRole(r *role.Role, holders RoleHolders) error {
	if err := a.MayDeactivateRole(r, holders); err != nil {
		return err
	}
	names := PermissionNames(r.Permissions())
	if err := roleMayContain(r, names); err != nil {
		return err
	}
	return a.mayConfer(names)
}

// MaySetRoleParent — ADR 0058 edges carry no authority (rank is level-only;
// the approver chain is reports_to) but must agree with the ladder: the actor
// may edit the child, and the parent is live, in the child's tenant and
// strictly more senior than the child. parent nil = clear.
func (a Actor) MaySetRoleParent(child, parent *role.Role) error {
	if err := a.mayMutateRole(child, NoLevelChange); err != nil {
		return err
	}
	if parent == nil {
		return nil
	}
	if !sameTenant(parent.TenantID(), child.TenantID()) {
		return ErrNoAuthority
	}
	return parentEligible(parent, levelOf(child))
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

// mayUnlinkManager - the hierarchy is strict: to remove or replace sub's
// current manager the actor must be that manager, or strictly outrank it (or be
// exempt). An equal-rank manager is not enough.
func (a Actor) mayUnlinkManager(sub, current Member) error {
	if !current.valid || !sameTenant(current.tenantID, sub.tenantID) {
		return ErrNoAuthority
	}
	if a.exemptIn(sub.tenantID) || current.id == a.membershipID || a.outranks(current.rank) {
		return nil
	}
	return fmt.Errorf("%w: the current manager is not more junior than you", ErrInsufficientRank)
}

// adminMustHold is the set of requestable permissions an Admin-tier approver
// may approve only while holding them itself (the owner's ruling: "Admin sits
// above all ... can approve everything", bounded so an Administrator whose owner
// deliberately restricted the role cannot hand out what it does not hold):
// authority over roles and permissions, tenant settings, money (marketplace
// purchase, recording payments) and tenant-wide visibility, plus the right to
// request elevation. Everything else requestable, notably the user-lifecycle
// names identity.users.{create,update,deactivate,reactivate,unlock}, is
// approvable by the Admin tier without holding it, so HR onboarding and
// offboarding approvals are not owner-only in tenants whose Administrator holds
// no tenant.admin. Owners hold tenant.admin, which implies all of these.
var adminMustHold = func() map[string]struct{} {
	p := permission.IdentityPermissions
	names := []string{
		p.Roles.Create, p.Roles.Update, p.Roles.Manage, p.Roles.Assign, p.Roles.Revoke, p.Roles.Approve,
		p.Users.UpdatePermissions,
		p.Tenants.Update, p.Tenants.UpdateSettings,
		p.PlatformMarketplace.Purchase, p.Orders.Payments.Record,
		p.Meta.RequestPermissionElevation, p.Users.View, p.Roles.View, p.Tenants.View,
		p.Crm.Leads.ReadAll, p.Tasks.WorkItems.ReadAll,
		p.Orders.Payments.Read, p.Orders.Invoices.Read, p.PlatformLeadCredits.Read,
	}
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}()

func mustHoldToApprove(name string) bool { _, ok := adminMustHold[name]; return ok }

// approvalNeedsHolding: does approving perm require the approver to hold it?
// The manager path always does; the Admin tier only for adminMustHold. It is the
// one definition mayDecide and CapApprovalExpiry share.
func (a Actor) approvalNeedsHolding(perm string) bool {
	return a.Rank() > RankAdmin || mustHoldToApprove(perm)
}

// CapApprovalExpiry - H4: an approval never outlives the approver's own grant.
// When approving perm REQUIRED the approver to hold it (see approvalNeedsHolding)
// and the approver holds it only through a time-bound override (the fresh read
// records that in the Actor), the approved grant ends no later than the
// approver's. Every other approver (held through a role or a permanent grant,
// not required to hold it, exempt in the requester's tenant) gets wanted back.
func (a Actor) CapApprovalExpiry(requester Member, perm string, wanted time.Time) time.Time {
	if !a.valid || a.exemptIn(requester.tenantID) || !a.approvalNeedsHolding(perm) {
		return wanted
	}
	if until, ok := a.holdUntil[perm]; ok && until.Before(wanted) {
		return until
	}
	return wanted
}

// DecidesWholeTenant - H4: may the actor decide requests of members who do not
// report to them? Exempt actors, and the Admin tier or above in their own tenant
// (never an impersonation session). Every other decider is the requester's
// manager (with identity.roles.approve), so the approver queue of such an actor
// is exactly the requests of their own reports.
func (a Actor) DecidesWholeTenant(tenantID tenant.ID) bool {
	switch {
	case !a.valid || a.impersonation:
		return false
	case a.exemptIn(tenantID):
		return true
	}
	return sameTenant(tenantID, a.tenantID) && a.Rank() <= RankAdmin
}

// MayDecideAny - H4: can the actor decide ANY request in tenantID at all? It is
// the cheap pre-check that lets the approver queue answer an empty page without
// scanning anything: false for an impersonation session, an actor of another
// tenant that is not exempt, and an actor below the Admin tier without
// identity.roles.approve. True does not mean any particular request is
// decidable; the per-request policy decides that.
func (a Actor) MayDecideAny(tenantID tenant.ID) bool {
	if a.DecidesWholeTenant(tenantID) {
		return true
	}
	return a.valid && !a.impersonation && sameTenant(tenantID, a.tenantID) && a.hasOwn(permission.IdentityPermissions.Roles.Approve)
}

// MayApprove - H4: (a) the requester's manager, who holds roles.approve and
// the permission; (b) any actor of Admin tier or above who strictly outranks
// the requester and approves any requestable permission without holding it,
// except the authority-and-money-and-visibility set (adminMustHold), which the
// Admin must hold ("admin sees through everything and approves day-to-day
// work"); (c) a super user or an exempt operator in another tenant. Never an
// impersonation session, never the requester; perm must be requestable.
func (a Actor) MayApprove(requester Member, perm string) error {
	return a.mayDecide(requester, perm, true)
}

// MayDeny - H4 eligibility without "holds the permission".
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
	case approving && !MayHoldAt(req.Rank(), perm):
		// Owner ruling 2026-11-21: nobody, an operator included, confers it below the tier.
		return fmt.Errorf("%w: %s may not be approved for rank %d", ErrAdminTierOnly, perm, req.Rank())
	case a.exemptIn(req.tenantID):
		return nil
	case !sameTenant(req.tenantID, a.tenantID):
		return ErrNoAuthority
	case !a.outranks(req.rank):
		return fmt.Errorf("%w: approver rank %d does not outrank requester rank %d", ErrInsufficientRank, a.Rank(), req.rank)
	case a.Rank() <= RankAdmin:
		// Admin sees and approves through day-to-day work without holding it,
		// but never confers authority or money it does not hold itself.
		if approving && a.approvalNeedsHolding(perm) && !a.holds(perm, req.tenantID) {
			return fmt.Errorf("%w: an admin must hold %s to approve it", ErrGrantExceedsAuthority, perm)
		}
		return nil
	case req.reportsTo != a.membershipID || !a.hasOwn(permission.IdentityPermissions.Roles.Approve):
		return ErrNotApprover
	case approving && !a.holds(perm, req.tenantID):
		return fmt.Errorf("%w: approver does not hold %s", ErrGrantExceedsAuthority, perm)
	}
	return nil
}

// GuardLastOwner - H6: refuse removing target's ownership (deactivation,
// CompanyOwner revocation) when no OTHER active owner of the same tenant
// remains. The target's own activity does not matter: a sole owner who is
// already suspended or deactivated is still the tenant's only owner.
// owners is every current member holding the CompanyOwner role, loaded in the
// same locked transaction; the active filter applies only to the OTHER owners
// (IsOwner and IsActive; the zero Member sets neither flag), so an inactive
// owner, a non-owner, an invalid Member or another tenant's owner never counts
// as "another owner". Applies to every actor.
func GuardLastOwner(target Member, owners []Member) error {
	if !target.valid {
		return ErrNoAuthority
	}
	if !target.owner {
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

// grantedNames is the names an override state grants.
func grantedNames(o Overrides) []string {
	out := make([]string, 0, len(o.Granted))
	for _, g := range o.Granted {
		out = append(out, g.Name)
	}
	return out
}
