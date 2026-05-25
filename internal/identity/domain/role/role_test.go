package role_test

import (
	"time"

	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// newRole builds a fresh Role with reasonable defaults — keeps the
// per-test arrange section short. Used by tests downstream of Task 6
// where the specific id/name/level don't matter.
func newRole(t *testing.T) *role.Role {
	t.Helper()
	r, err := role.New(
		role.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		"Sales Manager",
		false,
		role.HierarchyLevelDefault,
		false,
		testNow,
	)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	return r
}

func TestNew_AcceptsValidInputs(t *testing.T) {
	t.Parallel()
	r, err := role.New(
		role.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		"Sales Manager",
		false, role.HierarchyLevelDefault, false,
		testNow,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.IsSystemDefault() || r.IsSuperAdmin() {
		t.Fatal("non-default flags wrong")
	}
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(role.CreatedEvent); !ok {
		t.Fatalf("event type: %T", events[0])
	}
}

func TestNew_RejectsZeroID(t *testing.T) {
	t.Parallel()
	_, err := role.New(role.ID(""), tenant.ID(ids.NewV7().String()), "X", false, 50, false, testNow)
	if !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestNew_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(""), "X", false, 50, false, testNow)
	if !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestNew_RejectsBadName(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"", " ", "a"} {
		_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), n, false, 50, false, testNow)
		if !errors.Is(err, role.ErrInvalid) {
			t.Fatalf("name=%q want ErrInvalid got %v", n, err)
		}
	}
}

func TestNew_RejectsHierarchyOutOfRange(t *testing.T) {
	t.Parallel()
	for _, lvl := range []int{-1, 100, 200} {
		_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), "X", false, lvl, false, testNow)
		if !errors.Is(err, role.ErrInvalid) {
			t.Fatalf("lvl=%d should reject", lvl)
		}
	}
}

func TestRename_TransitionsAndEmits(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.PullEvents() // drop CreatedEvent
	if err := r.Rename("Senior Manager", testNow); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if r.Name() != "Senior Manager" {
		t.Fatalf("name: %q", r.Name())
	}
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(role.RenamedEvent)
	if !ok {
		t.Fatalf("event type: %T", events[0])
	}
	if ev.OldName != "Sales Manager" || ev.NewName != "Senior Manager" {
		t.Fatalf("event payload: %+v", ev)
	}
}

func TestRename_IdempotentNoEvent(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.PullEvents()
	if err := r.Rename("Sales Manager", testNow); err != nil {
		t.Fatalf("Rename same: %v", err)
	}
	if events := r.PullEvents(); len(events) != 0 {
		t.Fatalf("idempotent rename should emit 0 events, got %d", len(events))
	}
}

func TestRename_TrimsWhitespaceBeforeCompare(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.PullEvents()
	if err := r.Rename("  Sales Manager  ", testNow); err != nil {
		t.Fatalf("Rename trimmed-same: %v", err)
	}
	if events := r.PullEvents(); len(events) != 0 {
		t.Fatalf("rename to trimmed-same should emit 0 events, got %d", len(events))
	}
}

func TestRename_RejectsSystemDefault(t *testing.T) {
	t.Parallel()
	r, err := role.New(
		role.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		"TenantAdmin", true, 10, false,
		testNow,
	)
	if err != nil {
		t.Fatalf("New system-default: %v", err)
	}
	if err := r.Rename("Renamed", testNow); !errors.Is(err, role.ErrSystemDefault) {
		t.Fatalf("want ErrSystemDefault, got %v", err)
	}
}

func TestRename_RejectsBadName(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	for _, n := range []string{"", " ", "a"} {
		if err := r.Rename(n, testNow); !errors.Is(err, role.ErrInvalid) {
			t.Fatalf("name=%q: want ErrInvalid got %v", n, err)
		}
	}
}

func TestChangeHierarchyLevel_AcceptsValid(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	if err := r.ChangeHierarchyLevel(20); err != nil {
		t.Fatalf("ChangeHierarchyLevel: %v", err)
	}
	if r.HierarchyLevel() != 20 {
		t.Fatalf("level: got %d want 20", r.HierarchyLevel())
	}
}

func TestChangeHierarchyLevel_IdempotentNoChange(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	if err := r.ChangeHierarchyLevel(role.HierarchyLevelDefault); err != nil {
		t.Fatalf("ChangeHierarchyLevel same: %v", err)
	}
	// HierarchyLevel changes don't emit events (no event type defined
	// for it in this aggregate; mirror of .NET parent which emits no
	// event for level change).
}

func TestChangeHierarchyLevel_RejectsOutOfRange(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	for _, lvl := range []int{-1, 100, 500} {
		if err := r.ChangeHierarchyLevel(lvl); !errors.Is(err, role.ErrInvalid) {
			t.Fatalf("lvl=%d: want ErrInvalid got %v", lvl, err)
		}
	}
}

func TestChangeHierarchyLevel_RejectsSystemDefault(t *testing.T) {
	t.Parallel()
	r, _ := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()),
		"TenantAdmin", true, 10, false, testNow)
	if err := r.ChangeHierarchyLevel(20); !errors.Is(err, role.ErrSystemDefault) {
		t.Fatalf("want ErrSystemDefault got %v", err)
	}
}

func TestGrantPermission_AddsAndEmits(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.PullEvents()
	p := permission.FromConstant(permission.IdentityPermissions.Users.View)
	if err := r.GrantPermission(p, testNow); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !r.HasPermission(p) {
		t.Fatal("HasPermission false after Grant")
	}
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(role.PermissionGrantedEvent)
	if !ok {
		t.Fatalf("event type: %T", events[0])
	}
	if ev.Permission != "identity.users.view" {
		t.Fatalf("event.Permission: %q", ev.Permission)
	}
}

func TestGrantPermission_Idempotent(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.View)
	_ = r.GrantPermission(p, testNow) // arch-test:ignore-err - test fixture setup
	_ = r.PullEvents()
	if err := r.GrantPermission(p, testNow); err != nil {
		t.Fatalf("dup Grant: %v", err)
	}
	if events := r.PullEvents(); len(events) != 0 {
		t.Fatalf("idempotent dup emitted %d events", len(events))
	}
}

func TestGrantPermission_RejectsNil(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	if err := r.GrantPermission(nil, testNow); !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestRevokePermission_RemovesAndEmits(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	p := permission.FromConstant(permission.IdentityPermissions.Users.View)
	_ = r.GrantPermission(p, testNow) // arch-test:ignore-err - test fixture setup
	_ = r.PullEvents()
	if err := r.RevokePermission(p, testNow); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if r.HasPermission(p) {
		t.Fatal("HasPermission true after Revoke")
	}
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(role.PermissionRevokedEvent); !ok {
		t.Fatalf("event type: %T", events[0])
	}
}

func TestRevokePermission_NotPresentNoEvent(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.PullEvents()
	p := permission.FromConstant(permission.IdentityPermissions.Users.View)
	if err := r.RevokePermission(p, testNow); err != nil {
		t.Fatalf("Revoke missing: %v", err)
	}
	if events := r.PullEvents(); len(events) != 0 {
		t.Fatalf("revoke-missing should emit 0 events, got %d", len(events))
	}
}

func TestReplacePermissions_DiffEvents(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	view := permission.FromConstant(permission.IdentityPermissions.Users.View)
	create := permission.FromConstant(permission.IdentityPermissions.Users.Create)
	_ = r.GrantPermission(view, testNow) // arch-test:ignore-err - test fixture setup
	_ = r.PullEvents()

	if err := r.ReplacePermissions([]*permission.Permission{create}, testNow); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if r.HasPermission(view) {
		t.Fatal("view should be revoked")
	}
	if !r.HasPermission(create) {
		t.Fatal("create should be granted")
	}
	events := r.PullEvents()
	if len(events) != 2 {
		t.Fatalf("events: got %d want 2", len(events))
	}
	if _, ok := events[0].(role.PermissionRevokedEvent); !ok {
		t.Fatalf("first event should be Revoked, got %T", events[0])
	}
	if _, ok := events[1].(role.PermissionGrantedEvent); !ok {
		t.Fatalf("second event should be Granted, got %T", events[1])
	}
}

func TestReplacePermissions_EmptyTargetRevokesAll(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	view := permission.FromConstant(permission.IdentityPermissions.Users.View)
	create := permission.FromConstant(permission.IdentityPermissions.Users.Create)
	_ = r.GrantPermission(view, testNow) // arch-test:ignore-err - test fixture setup
	_ = r.GrantPermission(create, testNow) // arch-test:ignore-err - test fixture setup
	_ = r.PullEvents()

	if err := r.ReplacePermissions(nil, testNow); err != nil {
		t.Fatalf("Replace nil: %v", err)
	}
	if len(r.Permissions()) != 0 {
		t.Fatalf("permissions still present: %+v", r.Permissions())
	}
	events := r.PullEvents()
	if len(events) != 2 {
		t.Fatalf("events: got %d want 2 (both revoked)", len(events))
	}
}

func TestDelete_TransitionsAndEmits(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.PullEvents()
	if err := r.Delete("admin-1", testNow); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !r.IsDeleted() {
		t.Fatal("not deleted after Delete")
	}
	if r.DeletedBy() != "admin-1" {
		t.Fatalf("DeletedBy: %q", r.DeletedBy())
	}
	if r.DeletedAt().IsZero() {
		t.Fatal("DeletedAt should be set")
	}
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(role.DeletedEvent); !ok {
		t.Fatalf("event type: %T", events[0])
	}
}

func TestDelete_RejectsSystemDefault(t *testing.T) {
	t.Parallel()
	r, _ := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()),
		"TenantAdmin", true, 10, false, testNow)
	if err := r.Delete("admin-1", testNow); !errors.Is(err, role.ErrSystemDefault) {
		t.Fatalf("want ErrSystemDefault got %v", err)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	if err := r.Delete("admin-1", testNow); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	_ = r.PullEvents()
	if err := r.Delete("admin-1", testNow); err != nil {
		t.Fatalf("dup Delete: %v", err)
	}
	if events := r.PullEvents(); len(events) != 0 {
		t.Fatal("dup Delete should emit 0 events")
	}
}

func TestMutations_RejectAfterDelete(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.Delete("admin-1", testNow) // arch-test:ignore-err - test fixture setup
	p := permission.FromConstant(permission.IdentityPermissions.Users.View)
	if err := r.Rename("X", testNow); !errors.Is(err, role.ErrDeleted) {
		t.Fatalf("Rename after delete: %v", err)
	}
	if err := r.ChangeHierarchyLevel(20); !errors.Is(err, role.ErrDeleted) {
		t.Fatalf("ChangeHierarchyLevel after delete: %v", err)
	}
	if err := r.GrantPermission(p, testNow); !errors.Is(err, role.ErrDeleted) {
		t.Fatalf("Grant after delete: %v", err)
	}
	if err := r.RevokePermission(p, testNow); !errors.Is(err, role.ErrDeleted) {
		t.Fatalf("Revoke after delete: %v", err)
	}
	if err := r.ReplacePermissions(nil, testNow); !errors.Is(err, role.ErrDeleted) {
		t.Fatalf("Replace after delete: %v", err)
	}
}

func TestUnmarshalFromDB_RoundTripsAllFields(t *testing.T) {
	t.Parallel()
	view := permission.FromConstant(permission.IdentityPermissions.Users.View)
	rid := role.ID(ids.NewV7().String())
	tid := tenant.ID(ids.NewV7().String())
	r := role.UnmarshalFromDB(role.Snapshot{
		ID:              rid,
		TenantID:        tid,
		Name:            "Hydrated",
		IsSystemDefault: true,
		IsSuperAdmin:    false,
		HierarchyLevel:  25,
		Permissions:     []*permission.Permission{view},
		IsDeleted:       false,
	})
	if r.ID() != rid || r.TenantID() != tid {
		t.Fatalf("ids: %q / %q", r.ID(), r.TenantID())
	}
	if r.Name() != "Hydrated" || !r.IsSystemDefault() || r.IsSuperAdmin() {
		t.Fatalf("flags: name=%q sysdef=%v super=%v", r.Name(), r.IsSystemDefault(), r.IsSuperAdmin())
	}
	if r.HierarchyLevel() != 25 {
		t.Fatalf("hierarchy: %d", r.HierarchyLevel())
	}
	if !r.HasPermission(view) {
		t.Fatal("permissions did not round-trip")
	}
}

func TestUnmarshalFromDB_DoesNotEmitEvents(t *testing.T) {
	t.Parallel()
	r := role.UnmarshalFromDB(role.Snapshot{
		ID: role.ID(ids.NewV7().String()), TenantID: tenant.ID(ids.NewV7().String()),
		Name: "Hydrated", HierarchyLevel: 50,
	})
	if events := r.PullEvents(); len(events) != 0 {
		t.Fatalf("UnmarshalFromDB emitted %d events (must be 0 — TDL canon)", len(events))
	}
}


func TestHierarchyConstants(t *testing.T) {
	t.Parallel()
	if role.HierarchyLevelMin != 0 || role.HierarchyLevelMax != 99 ||
		role.HierarchyLevelDefault != 50 || role.HierarchyLevelNoRole != 99 {
		t.Fatal("hierarchy constants drifted from .NET parent")
	}
}

func TestID_IsZero(t *testing.T) {
	t.Parallel()
	if !role.ID("").IsZero() {
		t.Fatal("empty ID should be zero")
	}
	if role.ID("x").IsZero() {
		t.Fatal("non-empty ID should not be zero")
	}
}
