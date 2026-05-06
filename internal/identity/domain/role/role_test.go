package role_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

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
	_, err := role.New(role.ID(""), tenant.ID(ids.NewV7().String()), "X", false, 50, false)
	if !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestNew_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(""), "X", false, 50, false)
	if !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("want ErrInvalid got %v", err)
	}
}

func TestNew_RejectsBadName(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"", " ", "a"} {
		_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), n, false, 50, false)
		if !errors.Is(err, role.ErrInvalid) {
			t.Fatalf("name=%q want ErrInvalid got %v", n, err)
		}
	}
}

func TestNew_RejectsHierarchyOutOfRange(t *testing.T) {
	t.Parallel()
	for _, lvl := range []int{-1, 100, 200} {
		_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), "X", false, lvl, false)
		if !errors.Is(err, role.ErrInvalid) {
			t.Fatalf("lvl=%d should reject", lvl)
		}
	}
}

func TestRename_TransitionsAndEmits(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	_ = r.PullEvents() // drop CreatedEvent
	if err := r.Rename("Senior Manager"); err != nil {
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
	if err := r.Rename("Sales Manager"); err != nil {
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
	if err := r.Rename("  Sales Manager  "); err != nil {
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
	)
	if err != nil {
		t.Fatalf("New system-default: %v", err)
	}
	if err := r.Rename("Renamed"); !errors.Is(err, role.ErrSystemDefault) {
		t.Fatalf("want ErrSystemDefault, got %v", err)
	}
}

func TestRename_RejectsBadName(t *testing.T) {
	t.Parallel()
	r := newRole(t)
	for _, n := range []string{"", " ", "a"} {
		if err := r.Rename(n); !errors.Is(err, role.ErrInvalid) {
			t.Fatalf("name=%q: want ErrInvalid got %v", n, err)
		}
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
