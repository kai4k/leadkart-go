package rolehierarchy_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedClock returns a stable wall-clock for deterministic tests.
func fixedClock() time.Time {
	return time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
}

// validNew constructs a baseline valid Edge — every test starts here.
func validNew(t *testing.T) *rolehierarchy.Edge {
	t.Helper()
	e, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		"manager reorg requires this parent link",
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("rolehierarchy.New baseline: %v", err)
	}
	return e
}

func TestNew_RejectsSelfReference(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(ids.NewV7().String())
	same := role.ID(ids.NewV7().String())
	_, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tid,
		same,
		same,
		membership.ID(""),
		"",
		fixedClock(),
	)
	if !errors.Is(err, rolehierarchy.ErrSelfReference) {
		t.Fatalf("err = %v, want ErrSelfReference", err)
	}
}

func TestNew_RejectsShortReason(t *testing.T) {
	t.Parallel()
	_, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		membership.ID(""),
		"short",
		fixedClock(),
	)
	if !errors.Is(err, rolehierarchy.ErrInvalidReason) {
		t.Fatalf("err = %v, want ErrInvalidReason", err)
	}
}

func TestNew_AcceptsZeroReason(t *testing.T) {
	t.Parallel()
	// Reason is nullable in the schema; empty must succeed.
	e, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		membership.ID(""),
		"",
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("New with empty reason: %v", err)
	}
	if e.Reason() != "" {
		t.Errorf("Reason = %q, want empty", e.Reason())
	}
}

func TestNew_RejectsLongReason(t *testing.T) {
	t.Parallel()
	_, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		membership.ID(""),
		strings.Repeat("x", rolehierarchy.MaxReasonLength+1),
		fixedClock(),
	)
	if !errors.Is(err, rolehierarchy.ErrInvalidReason) {
		t.Fatalf("err = %v, want ErrInvalidReason", err)
	}
}

func TestNew_RecordsEstablishedEvent(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	events := e.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev, ok := events[0].(rolehierarchy.EstablishedEvent)
	if !ok {
		t.Fatalf("event type = %T, want EstablishedEvent", events[0])
	}
	if ev.ID != e.ID() {
		t.Errorf("event ID = %v, want %v", ev.ID, e.ID())
	}
	if ev.ChildRoleID != e.ChildRoleID() {
		t.Errorf("event ChildRoleID = %v, want %v", ev.ChildRoleID, e.ChildRoleID())
	}
}

func TestRemove_TransitionsToRemoved(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	if !e.IsActive() {
		t.Fatal("IsActive should be true before Remove")
	}
	remover := membership.ID(ids.NewV7().String())
	if err := e.Remove(remover, "manager rotation closed this link", fixedClock()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if e.IsActive() {
		t.Error("IsActive should be false after Remove")
	}
	if e.RemovedByMembershipID() != remover {
		t.Errorf("RemovedByMembershipID = %v, want %v", e.RemovedByMembershipID(), remover)
	}
}

func TestRemove_RejectsAlreadyRemoved(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	if err := e.Remove(membership.ID(ids.NewV7().String()), "first removal closes link", fixedClock()); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	err := e.Remove(membership.ID(ids.NewV7().String()), "second removal attempt", fixedClock())
	if !errors.Is(err, rolehierarchy.ErrNotActive) {
		t.Fatalf("err = %v, want ErrNotActive", err)
	}
}

func TestRemove_RecordsRemovedEvent(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	remover := membership.ID(ids.NewV7().String())
	if err := e.Remove(remover, "manager moved to another team", fixedClock()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	events := e.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev, ok := events[0].(rolehierarchy.RemovedEvent)
	if !ok {
		t.Fatalf("event type = %T, want RemovedEvent", events[0])
	}
	if ev.RemovedByMembershipID != remover {
		t.Errorf("event RemovedByMembershipID = %v, want %v", ev.RemovedByMembershipID, remover)
	}
}

func TestIsActive_TrueBeforeRemoval_FalseAfter(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	if !e.IsActive() {
		t.Error("IsActive before Remove = false, want true")
	}
	_ = e.PullEvents()
	if err := e.Remove(membership.ID(""), "", fixedClock()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if e.IsActive() {
		t.Error("IsActive after Remove = true, want false")
	}
}

func TestNew_RejectsShortReasonOnRemove(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	err := e.Remove(membership.ID(""), "short", fixedClock())
	if !errors.Is(err, rolehierarchy.ErrInvalidReason) {
		t.Fatalf("err = %v, want ErrInvalidReason", err)
	}
}
