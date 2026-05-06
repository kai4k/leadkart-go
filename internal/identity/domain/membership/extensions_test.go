package membership_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// freshMembership builds a brand-new Active Membership and drains the
// CreatedEvent so per-test event assertions start from a clean slate.
func freshMembership(t *testing.T) *membership.Membership {
	t.Helper()
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		person.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	_ = m.PullEvents()
	return m
}

// ----- AssignRole / RevokeRole ----------------------------------------------

func TestAssignRole_AddsAndEmits(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	rid := role.ID(ids.NewV7().String())
	if err := m.AssignRole(rid); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	got := m.RoleAssignments()
	if len(got) != 1 || got[0] != rid {
		t.Fatalf("RoleAssignments: %v", got)
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(membership.RoleAssignedEvent)
	if !ok {
		t.Fatalf("event type: %T", events[0])
	}
	if ev.RoleID != rid {
		t.Fatalf("event RoleID: %v", ev.RoleID)
	}
}

func TestAssignRole_Idempotent(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	rid := role.ID(ids.NewV7().String())
	_ = m.AssignRole(rid)
	_ = m.PullEvents()
	if err := m.AssignRole(rid); err != nil {
		t.Fatalf("dup AssignRole: %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("dup AssignRole emitted %d events", len(events))
	}
	if got := m.RoleAssignments(); len(got) != 1 {
		t.Fatalf("RoleAssignments after dup: %v (want 1)", got)
	}
}

func TestAssignRole_RejectsZeroID(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	if err := m.AssignRole(role.ID("")); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestRevokeRole_RemovesAndEmits(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	rid := role.ID(ids.NewV7().String())
	_ = m.AssignRole(rid)
	_ = m.PullEvents()

	if err := m.RevokeRole(rid); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	if got := m.RoleAssignments(); len(got) != 0 {
		t.Fatalf("RoleAssignments still has %d entries", len(got))
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(membership.RoleRevokedEvent); !ok {
		t.Fatalf("event type: %T", events[0])
	}
}

func TestRevokeRole_NotPresentNoEvent(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	rid := role.ID(ids.NewV7().String())
	if err := m.RevokeRole(rid); err != nil {
		t.Fatalf("RevokeRole missing: %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("revoke-missing emitted %d events", len(events))
	}
}

func TestRoleAssignments_DefensiveCopy(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	rid := role.ID(ids.NewV7().String())
	_ = m.AssignRole(rid)
	got := m.RoleAssignments()
	got[0] = role.ID("mutated")
	if m.RoleAssignments()[0] == "mutated" {
		t.Fatal("mutating returned slice leaked into aggregate state")
	}
}

// ----- EffectivePermissions resolver -----------------------------------------

func TestEffectivePermissions_UnionRolesPlusGrantsMinusRevokes(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)

	// Build two roles. Manager grants View+Update; Auditor grants View only.
	managerRole, err := role.New(
		role.ID(ids.NewV7().String()), m.TenantID(),
		"Manager", false, role.HierarchyLevelDefault, false,
	)
	if err != nil {
		t.Fatalf("Manager role: %v", err)
	}
	_ = managerRole.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.View))
	_ = managerRole.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.Update))

	auditorRole, err := role.New(
		role.ID(ids.NewV7().String()), m.TenantID(),
		"Auditor", false, 60, false,
	)
	if err != nil {
		t.Fatalf("Auditor role: %v", err)
	}
	_ = auditorRole.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.View))

	// Membership overlay: grant Anonymise, revoke Update.
	_ = m.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.Anonymise))
	_ = m.RevokePermission(permission.FromConstant(permission.IdentityPermissions.Users.Update))

	got := m.EffectivePermissions([]*role.Role{managerRole, auditorRole})
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p.Name()] = true
	}

	if !gotSet["identity.users.view"] {
		t.Fatal("union missing View (granted by both roles)")
	}
	if !gotSet["identity.users.anonymise"] {
		t.Fatal("union missing overlay-granted Anonymise")
	}
	if gotSet["identity.users.update"] {
		t.Fatal("union should NOT include overlay-revoked Update")
	}
}

func TestEffectivePermissions_NoRolesEqualsOverlayOnly(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.View)
	_ = m.GrantPermission(p)

	got := m.EffectivePermissions(nil)
	if len(got) != 1 || !got[0].Equal(p) {
		t.Fatalf("got %+v want overlay-only [View]", got)
	}
}

func TestEffectivePermissions_RevokeWinsOverRoleGrant(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	r, err := role.New(role.ID(ids.NewV7().String()), m.TenantID(), "Sales", false, 50, false)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	view := permission.FromConstant(permission.IdentityPermissions.Users.View)
	_ = r.GrantPermission(view)
	_ = m.RevokePermission(view) // overlay revoke beats role grant

	got := m.EffectivePermissions([]*role.Role{r})
	for _, p := range got {
		if p.Equal(view) {
			t.Fatal("overlay revoke should suppress role grant")
		}
	}
}

// ----- Permission overlay (Granted/Revoked) ----------------------------------

func TestGrantPermission_AddsToOverlay(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	if err := m.GrantPermission(p); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	got := m.GrantedPermissions()
	if len(got) != 1 || !got[0].Equal(p) {
		t.Fatalf("GrantedPermissions: %+v", got)
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(membership.PermissionsUpdatedEvent); !ok {
		t.Fatalf("event type: %T", events[0])
	}
}

func TestGrantPermission_RemovesFromRevokedIfPreviouslyRevoked(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	_ = m.RevokePermission(p)
	_ = m.PullEvents()

	if err := m.GrantPermission(p); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if len(m.RevokedPermissions()) != 0 {
		t.Fatal("revoked overlay still has entry after grant")
	}
	if len(m.GrantedPermissions()) != 1 {
		t.Fatal("granted overlay missing entry after grant")
	}
}

func TestRevokePermission_RemovesFromGrantedIfPreviouslyGranted(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	_ = m.GrantPermission(p)
	_ = m.PullEvents()

	if err := m.RevokePermission(p); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(m.GrantedPermissions()) != 0 {
		t.Fatal("granted overlay still has entry after revoke")
	}
	if len(m.RevokedPermissions()) != 1 {
		t.Fatal("revoked overlay missing entry after revoke")
	}
}

func TestGrantPermission_Idempotent(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	_ = m.GrantPermission(p)
	_ = m.PullEvents()
	if err := m.GrantPermission(p); err != nil {
		t.Fatalf("dup Grant: %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("dup Grant emitted %d events", len(events))
	}
}

func TestRevokePermission_Idempotent(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	_ = m.RevokePermission(p)
	_ = m.PullEvents()
	if err := m.RevokePermission(p); err != nil {
		t.Fatalf("dup Revoke: %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("dup Revoke emitted %d events", len(events))
	}
}

func TestGrantPermission_RejectsNil(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	if err := m.GrantPermission(nil); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestRevokePermission_RejectsNil(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	if err := m.RevokePermission(nil); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestReplacePermissionOverlays_SetsAtomically(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	view := permission.FromConstant(permission.IdentityPermissions.Users.View)
	create := permission.FromConstant(permission.IdentityPermissions.Users.Create)
	anonymise := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	_ = m.GrantPermission(view) // pre-existing state
	_ = m.PullEvents()

	if err := m.ReplacePermissionOverlays(
		[]*permission.Permission{create, anonymise},
		[]*permission.Permission{view},
	); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(m.GrantedPermissions()) != 2 {
		t.Fatalf("granted: %d want 2", len(m.GrantedPermissions()))
	}
	if len(m.RevokedPermissions()) != 1 {
		t.Fatalf("revoked: %d want 1", len(m.RevokedPermissions()))
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d want 1 (single PermissionsUpdated)", len(events))
	}
	if _, ok := events[0].(membership.PermissionsUpdatedEvent); !ok {
		t.Fatalf("event type: %T", events[0])
	}
}

func TestUnmarshalFromDB_OverlayRoundTrip(t *testing.T) {
	t.Parallel()
	g := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	r := permission.FromConstant(permission.IdentityPermissions.Users.Update)
	m := membership.UnmarshalFromDB(membership.Snapshot{
		ID:                 membership.ID(ids.NewV7().String()),
		PersonID:           person.ID(ids.NewV7().String()),
		TenantID:           tenant.ID(ids.NewV7().String()),
		Status:             membership.StatusActive,
		GrantedPermissions: []*permission.Permission{g},
		RevokedPermissions: []*permission.Permission{r},
	})
	if got := m.GrantedPermissions(); len(got) != 1 || !got[0].Equal(g) {
		t.Fatalf("granted round-trip: %+v", got)
	}
	if got := m.RevokedPermissions(); len(got) != 1 || !got[0].Equal(r) {
		t.Fatalf("revoked round-trip: %+v", got)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("UnmarshalFromDB emitted %d events", len(events))
	}
}

func TestUnmarshalFromDB_RoleAssignmentsRoundTrip(t *testing.T) {
	t.Parallel()
	rid := role.ID(ids.NewV7().String())
	m := membership.UnmarshalFromDB(membership.Snapshot{
		ID:              membership.ID(ids.NewV7().String()),
		PersonID:        person.ID(ids.NewV7().String()),
		TenantID:        tenant.ID(ids.NewV7().String()),
		Status:          membership.StatusActive,
		RoleAssignments: []role.ID{rid},
	})
	got := m.RoleAssignments()
	if len(got) != 1 || got[0] != rid {
		t.Fatalf("round-trip: %v", got)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("UnmarshalFromDB emitted %d events", len(events))
	}
}
