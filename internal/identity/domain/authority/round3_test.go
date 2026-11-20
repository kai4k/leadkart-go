package authority_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// adminMustHoldNames is the authority-and-money set an Admin-tier approver may
// approve only while holding it (restated here on purpose: the test pins the
// list, the policy owns it).
func adminMustHoldNames() map[string]bool {
	p := permission.IdentityPermissions
	out := map[string]bool{}
	for _, n := range []string{
		p.Roles.Create, p.Roles.Update, p.Roles.Delete, p.Roles.Assign, p.Roles.Revoke, p.Roles.Approve,
		p.Users.UpdatePermissions,
		p.Tenants.Update, p.Tenants.UpdateSettings,
		p.PlatformMarketplace.Purchase, p.Orders.Payments.Record,
		// visibility and the right to ask (hierarchy round 5)
		p.Meta.RequestPermissionElevation, p.Users.View, p.Roles.View, p.Tenants.View,
		p.Crm.Leads.ReadAll, p.Tasks.WorkItems.ReadAll,
		p.Orders.Payments.Read, p.Orders.Invoices.Read, p.PlatformLeadCredits.Read,
	} {
		out[n] = true
	}
	return out
}

// TestApproval_AdminMustHoldAuthorityAndMoney: an Admin approves day-to-day
// work without holding it, but must hold every permission in the
// authority-and-money set. Every requestable permission is checked on both
// sides of the list.
func TestApproval_AdminMustHoldAuthorityAndMoney(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false)
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	admin := mkMember(t, 13, tA, true, "", adminR)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	user := mkMember(t, 18, tA, true, "", userR)
	aAdminHoldsNothing := actorOf(t, admin, authority.RankAdmin)
	aOwner := actorOf(t, owner, authority.RankOwner, p.Meta.TenantAdmin)

	list := adminMustHoldNames()
	inList, outOfList := 0, 0
	for _, perm := range permission.All() {
		n := perm.Name()
		if !permission.IsRequestable(n) {
			if list[n] {
				t.Errorf("%s is in the must-hold list but is not requestable", n)
			}
			continue
		}
		heldAdmin := actorOf(t, admin, authority.RankAdmin, n) // an Admin that holds exactly this permission
		if authority.IsAdminTierOnly(n) {
			// Owner ruling 2026-11-21: identity.users.create is not approvable for a
			// requester below the Administrator tier by anyone (this requester is rank 50).
			for who, a := range map[string]authority.Actor{"admin without it": aAdminHoldsNothing, "admin with it": heldAdmin, "owner": aOwner} {
				if err := a.MayApprove(user, n); !errors.Is(err, authority.ErrAdminTierOnly) {
					t.Errorf("%s approves %s for a rank-50 requester: %v, want ErrAdminTierOnly", who, n, err)
				}
			}
			outOfList++
			continue
		}
		if list[n] {
			inList++
			if err := aAdminHoldsNothing.MayApprove(user, n); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
				t.Errorf("admin without %s approves it: %v, want ErrGrantExceedsAuthority", n, err)
			}
			if err := heldAdmin.MayApprove(user, n); err != nil {
				t.Errorf("admin holding %s is refused: %v", n, err)
			}
		} else {
			outOfList++
			if err := aAdminHoldsNothing.MayApprove(user, n); err != nil {
				t.Errorf("admin without %s is refused day-to-day work: %v", n, err)
			}
		}
		if err := aOwner.MayApprove(user, n); err != nil {
			t.Errorf("owner approving %s: %v", n, err)
		}
	}
	if inList != len(list) || outOfList == 0 {
		t.Fatalf("catalogue coverage: %d of %d list names seen, %d outside", inList, len(list), outOfList)
	}
	// Deny never needs the permission.
	if err := aAdminHoldsNothing.MayDeny(user); err != nil {
		t.Errorf("admin denies: %v", err)
	}
}

// TestOverrides_SnapshotDecidesNotTheCaller: the "before" is what MemberOf
// loaded. A member holds a 7-day grant of X; an actor who outranks the member
// but does not hold X sends the same list back with X permanent (or later):
// that is a change to X, so the ceiling applies. The same list with the same
// expiry is no change.
func TestOverrides_SnapshotDecidesNotTheCaller(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Crm.Leads.Assign
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	aMgr := actorOf(t, mgr, authority.RankManager, p.Crm.Leads.Read) // outranks the target, does not hold X

	week := t0.Add(7 * 24 * time.Hour)
	timed := mkMemberOv(t, 18, tA, []membership.GrantedOverride{{Permission: permission.FromConstant(x), ExpiresAt: week}}, nil, userR)
	expired := mkMemberOv(t, 19, tA, []membership.GrantedOverride{{Permission: permission.FromConstant(x), ExpiresAt: t0.Add(-time.Hour)}}, nil, userR)
	boundary := mkMemberOv(t, 20, tA, []membership.GrantedOverride{{Permission: permission.FromConstant(x), ExpiresAt: t0}}, nil, userR) // expires exactly now: expired
	permanentX := mkMemberOv(t, 21, tA, []membership.GrantedOverride{permanent(x)}, nil, userR)

	cases := []decision{
		{"same list, same expiry: no change", aMgr.MayChangeOverrides(timed, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: week}}}), nil},
		{"same list sent back permanent: the grant is extended", aMgr.MayChangeOverrides(timed, authority.Overrides{Granted: grants(x)}), authority.ErrGrantExceedsAuthority},
		{"expiry pushed later", aMgr.MayChangeOverrides(timed, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: week.Add(time.Hour)}}}), authority.ErrGrantExceedsAuthority},
		{"expiry pulled earlier is a change too", aMgr.MayChangeOverrides(timed, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: week.Add(-time.Hour)}}}), authority.ErrGrantExceedsAuthority},
		{"the grant dropped", aMgr.MayChangeOverrides(timed, authority.Overrides{}), authority.ErrGrantExceedsAuthority},
		{"two entries for X with different expiries", aMgr.MayChangeOverrides(timed, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: week}, {Name: x}}}), authority.ErrGrantExceedsAuthority},
		{"an expired grant listed again is a new grant", aMgr.MayChangeOverrides(expired, authority.Overrides{Granted: grants(x)}), authority.ErrGrantExceedsAuthority},
		{"a grant expiring exactly now is expired", aMgr.MayChangeOverrides(boundary, authority.Overrides{Granted: grants(x)}), authority.ErrGrantExceedsAuthority},
		{"a grant expiring exactly now is not carried over as live", aMgr.MayChangeOverrides(boundary, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: t0}}}), authority.ErrGrantExceedsAuthority},
		{"an expired grant needs no ceiling to leave out", aMgr.MayChangeOverrides(expired, authority.Overrides{}), nil},
		{"a permanent grant sent back unchanged", aMgr.MayChangeOverrides(permanentX, authority.Overrides{Granted: grants(x)}), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
	snap := timed.Overrides()
	if len(snap.Granted) != 1 || snap.Granted[0].Name != x || !snap.Granted[0].ExpiresAt.Equal(week) {
		t.Errorf("snapshot of a time-bound grant: %+v", snap)
	}
	if got := expired.Overrides(); len(got.Granted) != 0 {
		t.Errorf("snapshot kept an expired grant: %+v", got)
	}
	snap.Granted[0].Name = "mutated"
	if timed.Overrides().Granted[0].Name != x {
		t.Error("Overrides() returned the member's own slice")
	}
}

// TestPersonState: the zero value cannot act; the constructor is the only way
// to say a person can.
func TestPersonState(t *testing.T) {
	t.Parallel()
	now := t0
	cases := []struct {
		name               string
		active, susp, anon bool
		want               bool
	}{
		{"active", true, false, false, true},
		{"deactivated", false, false, false, false},
		{"globally suspended", true, true, false, false},
		{"anonymised", true, false, true, false},
		{"suspended and anonymised", true, true, true, false},
		{"deactivated and suspended", false, true, false, false},
	}
	for _, c := range cases {
		if got := authority.NewPersonState(c.active, c.susp, c.anon).CanAct(); got != c.want {
			t.Errorf("%s: CanAct = %v, want %v", c.name, got, c.want)
		}
	}
	var zero authority.PersonState
	if zero.CanAct() {
		t.Error("zero PersonState can act")
	}
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	m := mkMembership(18, tA, true, "", userR)
	got, err := authority.MemberOf(m, []*role.Role{userR}, now, zero)
	if err != nil || got.IsActive() {
		t.Errorf("unknown person state: active=%v err=%v, want an inactive member", got.IsActive(), err)
	}
}
