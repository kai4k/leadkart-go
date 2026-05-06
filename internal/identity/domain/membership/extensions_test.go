package membership_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
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
