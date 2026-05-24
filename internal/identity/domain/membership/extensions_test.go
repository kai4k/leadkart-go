package membership_test

import (
	"errors"
	"testing"
	"time"

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
		membership.ID(""),
		testNow,
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
	if err := m.AssignRole(rid, testNow); err != nil {
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
	_ = m.AssignRole(rid, testNow)
	_ = m.PullEvents()
	if err := m.AssignRole(rid, testNow); err != nil {
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
	if err := m.AssignRole(role.ID(""), testNow); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestRevokeRole_RemovesAndEmits(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	rid := role.ID(ids.NewV7().String())
	_ = m.AssignRole(rid, testNow)
	_ = m.PullEvents()

	if err := m.RevokeRole(rid, testNow); err != nil {
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
	if err := m.RevokeRole(rid, testNow); err != nil {
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
	_ = m.AssignRole(rid, testNow)
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
		testNow,
	)
	if err != nil {
		t.Fatalf("Manager role: %v", err)
	}
	_ = managerRole.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.View), testNow)
	_ = managerRole.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.Update), testNow)

	auditorRole, err := role.New(
		role.ID(ids.NewV7().String()), m.TenantID(),
		"Auditor", false, 60, false,
		testNow,
	)
	if err != nil {
		t.Fatalf("Auditor role: %v", err)
	}
	_ = auditorRole.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.View), testNow)

	// Membership overlay: grant Anonymise, revoke Update.
	_ = m.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.Anonymise), time.Time{}, testNow)
	_ = m.RevokePermission(permission.FromConstant(permission.IdentityPermissions.Users.Update), testNow)

	got := m.EffectivePermissions([]*role.Role{managerRole, auditorRole}, time.Now())
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
	_ = m.GrantPermission(p, time.Time{}, testNow)

	got := m.EffectivePermissions(nil, time.Now())
	if len(got) != 1 || !got[0].Equal(p) {
		t.Fatalf("got %+v want overlay-only [View]", got)
	}
}

func TestEffectivePermissions_RevokeWinsOverRoleGrant(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	r, err := role.New(role.ID(ids.NewV7().String()), m.TenantID(), "Sales", false, 50, false, testNow)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	view := permission.FromConstant(permission.IdentityPermissions.Users.View)
	_ = r.GrantPermission(view, testNow)
	_ = m.RevokePermission(view, testNow) // overlay revoke beats role grant

	got := m.EffectivePermissions([]*role.Role{r}, time.Now())
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
	if err := m.GrantPermission(p, time.Time{}, testNow); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	got := m.GrantedPermissions()
	if len(got) != 1 || !got[0].Permission.Equal(p) {
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
	_ = m.RevokePermission(p, testNow)
	_ = m.PullEvents()

	if err := m.GrantPermission(p, time.Time{}, testNow); err != nil {
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
	_ = m.GrantPermission(p, time.Time{}, testNow)
	_ = m.PullEvents()

	if err := m.RevokePermission(p, testNow); err != nil {
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
	_ = m.GrantPermission(p, time.Time{}, testNow)
	_ = m.PullEvents()
	if err := m.GrantPermission(p, time.Time{}, testNow); err != nil {
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
	_ = m.RevokePermission(p, testNow)
	_ = m.PullEvents()
	if err := m.RevokePermission(p, testNow); err != nil {
		t.Fatalf("dup Revoke: %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("dup Revoke emitted %d events", len(events))
	}
}

func TestGrantPermission_RejectsNil(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	if err := m.GrantPermission(nil, time.Time{}, testNow); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestRevokePermission_RejectsNil(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	if err := m.RevokePermission(nil, testNow); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

// ADR 0055 — time-bound overlay grants. GrantPermission accepts an
// optional expiry; the resolver filters expired entries at resolve time.
func TestGrantPermission_WithExpiresAt(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	expiresAt := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	if err := m.GrantPermission(p, expiresAt, testNow); err != nil {
		t.Fatalf("GrantPermission with expiry: %v", err)
	}
	got := m.GrantedPermissions()
	if len(got) != 1 {
		t.Fatalf("granted len = %d, want 1", len(got))
	}
	if !got[0].ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got[0].ExpiresAt, expiresAt)
	}
}

func TestEffectivePermissions_FiltersExpiredOverrides(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	// Grant with an expiry 1 hour ago — already expired.
	expiredAt := time.Date(2026, 5, 23, 11, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	_ = m.GrantPermission(p, expiredAt, testNow)

	got := m.EffectivePermissions(nil, now)
	for _, gp := range got {
		if gp.Equal(p) {
			t.Fatalf("expired overlay grant should be filtered out at resolve time")
		}
	}
}

func TestEffectivePermissions_KeepsUnexpiredOverrides(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	// Grant with an expiry 1 hour in the future — still active.
	expiresAt := time.Date(2026, 5, 23, 13, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	_ = m.GrantPermission(p, expiresAt, testNow)

	got := m.EffectivePermissions(nil, now)
	if len(got) != 1 || !got[0].Equal(p) {
		t.Fatalf("unexpired overlay grant should be present; got %+v", got)
	}
}

func TestEffectivePermissions_PerpetualOverridesNeverExpire(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	// time.Time{} = perpetual per ADR 0055.
	_ = m.GrantPermission(p, time.Time{}, testNow)

	// Even with `now` at the heat-death-of-the-universe the grant holds.
	now := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	got := m.EffectivePermissions(nil, now)
	if len(got) != 1 || !got[0].Equal(p) {
		t.Fatalf("perpetual overlay grant should always be present; got %+v", got)
	}
}

func TestReplacePermissionOverlays_SetsAtomically(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	view := permission.FromConstant(permission.IdentityPermissions.Users.View)
	create := permission.FromConstant(permission.IdentityPermissions.Users.Create)
	anonymise := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	_ = m.GrantPermission(view, time.Time{}, testNow) // pre-existing state
	_ = m.PullEvents()

	if err := m.ReplacePermissionOverlays(
		[]*permission.Permission{create, anonymise},
		[]*permission.Permission{view},
		testNow,
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
		GrantedPermissions: []membership.GrantedOverride{{Permission: g}},
		RevokedPermissions: []*permission.Permission{r},
	})
	if got := m.GrantedPermissions(); len(got) != 1 || !got[0].Permission.Equal(g) {
		t.Fatalf("granted round-trip: %+v", got)
	}
	if got := m.RevokedPermissions(); len(got) != 1 || !got[0].Equal(r) {
		t.Fatalf("revoked round-trip: %+v", got)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("UnmarshalFromDB emitted %d events", len(events))
	}
}

// ----- Profile + manager hierarchy -------------------------------------------

func TestUpdateProfile_TransitionsAndEmits(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	if err := m.UpdateProfile("Sales Lead", "Sales", "OOO Friday", testNow); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if m.Designation() != "Sales Lead" || m.Department() != "Sales" || m.StatusMessage() != "OOO Friday" {
		t.Fatalf("fields: %q/%q/%q", m.Designation(), m.Department(), m.StatusMessage())
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(membership.ProfileUpdatedEvent)
	if !ok {
		t.Fatalf("event type: %T", events[0])
	}
	if ev.Designation != "Sales Lead" || ev.Department != "Sales" || ev.StatusMessage != "OOO Friday" {
		t.Fatalf("event payload: %+v", ev)
	}
}

func TestUpdateProfile_TrimsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	_ = m.UpdateProfile("Lead", "Sales", "", testNow)
	_ = m.PullEvents()
	if err := m.UpdateProfile("  Lead  ", "  Sales  ", "", testNow); err != nil {
		t.Fatalf("UpdateProfile (whitespace): %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("idempotent UpdateProfile emitted %d events", len(events))
	}
}

func TestAssignManager_TransitionsAndEmits(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	mgr := membership.ID(ids.NewV7().String())
	if err := m.AssignManager(mgr, testNow); err != nil {
		t.Fatalf("AssignManager: %v", err)
	}
	if m.ReportsTo() != mgr {
		t.Fatalf("ReportsTo: %v", m.ReportsTo())
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(membership.ManagerAssignedEvent)
	if !ok {
		t.Fatalf("event type: %T", events[0])
	}
	if ev.ManagerID != mgr {
		t.Fatalf("event ManagerID: %v", ev.ManagerID)
	}
	if !ev.PreviousManager.IsZero() {
		t.Fatalf("event PreviousManager: %v (want zero)", ev.PreviousManager)
	}
}

func TestAssignManager_RejectsSelfReference(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	if err := m.AssignManager(m.ID(), testNow); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestAssignManager_ZeroIDClears(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	prev := membership.ID(ids.NewV7().String())
	_ = m.AssignManager(prev, testNow)
	_ = m.PullEvents()
	if err := m.AssignManager(membership.ID(""), testNow); err != nil {
		t.Fatalf("clear manager: %v", err)
	}
	if !m.ReportsTo().IsZero() {
		t.Fatalf("ReportsTo not cleared: %v", m.ReportsTo())
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(membership.ManagerRemovedEvent)
	if !ok {
		t.Fatalf("event type: %T (want ManagerRemovedEvent)", events[0])
	}
	if ev.PreviousManager != prev {
		t.Fatalf("event PreviousManager: %v want %v", ev.PreviousManager, prev)
	}
}

// RemoveManager is the semantic-clear counterpart of AssignManager —
// callers reading "remove the manager" shouldn't have to spell that
// as "assign the zero ID." Mirrors the .NET LeadKart MembershipManager
// remove command shape per messaging.md "Identity event vocabulary."
func TestRemoveManager_ClearsReportsTo_AndEmitsManagerRemovedEvent(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	prev := membership.ID(ids.NewV7().String())
	_ = m.AssignManager(prev, testNow)
	_ = m.PullEvents()

	if err := m.RemoveManager(testNow); err != nil {
		t.Fatalf("RemoveManager: %v", err)
	}
	if !m.ReportsTo().IsZero() {
		t.Fatalf("ReportsTo not cleared: %v", m.ReportsTo())
	}
	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(membership.ManagerRemovedEvent)
	if !ok {
		t.Fatalf("event type: %T (want ManagerRemovedEvent)", events[0])
	}
	if ev.PreviousManager != prev {
		t.Fatalf("event PreviousManager: %v want %v", ev.PreviousManager, prev)
	}
}

func TestRemoveManager_NoOp_WhenNoManagerSet(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	// Fresh membership has no manager. RemoveManager must succeed
	// silently and emit no event (idempotent like AssignManager).
	if err := m.RemoveManager(testNow); err != nil {
		t.Fatalf("RemoveManager on cleared: %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("RemoveManager on cleared emitted %d events", len(events))
	}
}

func TestAssignManager_Idempotent(t *testing.T) {
	t.Parallel()
	m := freshMembership(t)
	mgr := membership.ID(ids.NewV7().String())
	_ = m.AssignManager(mgr, testNow)
	_ = m.PullEvents()
	if err := m.AssignManager(mgr, testNow); err != nil {
		t.Fatalf("dup AssignManager: %v", err)
	}
	if events := m.PullEvents(); len(events) != 0 {
		t.Fatalf("dup AssignManager emitted %d events", len(events))
	}
}

func TestUnmarshalFromDB_ProfileAndManagerRoundTrip(t *testing.T) {
	t.Parallel()
	mgr := membership.ID(ids.NewV7().String())
	m := membership.UnmarshalFromDB(membership.Snapshot{
		ID:            membership.ID(ids.NewV7().String()),
		PersonID:      person.ID(ids.NewV7().String()),
		TenantID:      tenant.ID(ids.NewV7().String()),
		Status:        membership.StatusActive,
		Designation:   "Lead",
		Department:    "Sales",
		StatusMessage: "Available",
		ReportsTo:     mgr,
	})
	if m.Designation() != "Lead" || m.Department() != "Sales" || m.StatusMessage() != "Available" {
		t.Fatalf("profile: %q/%q/%q", m.Designation(), m.Department(), m.StatusMessage())
	}
	if m.ReportsTo() != mgr {
		t.Fatalf("ReportsTo round-trip: %v", m.ReportsTo())
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
