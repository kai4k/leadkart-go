package membership_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func newMembershipID(t *testing.T) membership.ID {
	t.Helper()
	return membership.ID(ids.NewV7().String())
}

func newPersonID(t *testing.T) person.ID {
	t.Helper()
	return person.ID(ids.NewV7().String())
}

func newTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

// ----- Factory: New ---------------------------------------------------------

func TestNewMembership_AcceptsValid_StartsActive(t *testing.T) {

	id := newMembershipID(t)
	pid := newPersonID(t)
	tid := newTenantID(t)

	m, err := membership.New(id, pid, tid, membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m == nil {
		t.Fatal("New returned nil")
	}
	if m.ID() != id {
		t.Errorf("ID() mismatch")
	}
	if m.PersonID() != pid {
		t.Errorf("PersonID() mismatch")
	}
	if m.TenantID() != tid {
		t.Errorf("TenantID() mismatch")
	}
	if m.Status() != membership.StatusActive {
		t.Errorf("Status() = %v, want StatusActive", m.Status())
	}
	if !m.JoinedAt().Equal(testNow) {
		t.Errorf("JoinedAt = %v", m.JoinedAt())
	}
	if !m.LeftAt().IsZero() {
		t.Errorf("LeftAt should be zero on new active membership, got %v", m.LeftAt())
	}
}

func TestNewMembership_EmitsCreatedEvent(t *testing.T) {

	id := newMembershipID(t)
	pid := newPersonID(t)
	tid := newTenantID(t)
	m, err := membership.New(id, pid, tid, membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	created, ok := events[0].(membership.CreatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want CreatedEvent", events[0])
	}
	if created.MembershipID != id {
		t.Errorf("event MembershipID = %v, want %v", created.MembershipID, id)
	}
	if created.PersonID != pid {
		t.Errorf("event PersonID mismatch")
	}
	if tenancy.ID(created.TenantID) != tenancy.ID(tid.String()) {
		t.Errorf("event TenantID mismatch")
	}
}

func TestNewMembership_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   membership.ID
		pid  person.ID
		tid  tenant.ID
	}{
		{"zero id", membership.ID(""), newPersonID(t), newTenantID(t)},
		{"zero personID", newMembershipID(t), person.ID(""), newTenantID(t)},
		{"zero tenantID", newMembershipID(t), newPersonID(t), tenant.ID("")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := membership.New(tc.id, tc.pid, tc.tid, membership.ID(""), testNow)
			if err == nil {
				t.Fatal("expected error")
			}
			if errs.KindOf(err) != errs.KindInvalidInput {
				t.Errorf("Kind = %v, want KindInvalidInput", errs.KindOf(err))
			}
			if !errors.Is(err, membership.ErrInvalid) {
				t.Errorf("expected errors.Is ErrInvalid")
			}
		})
	}
}

// ----- Deactivate -----------------------------------------------------------

func TestDeactivate_FromActive_TransitionsToInactive(t *testing.T) {

	m := newMembership(t)
	_ = m.PullEvents()

	if err := m.Deactivate("job change", testNow); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if m.Status() != membership.StatusInactive {
		t.Errorf("Status = %v, want StatusInactive", m.Status())
	}
	if !m.LeftAt().Equal(testNow) {
		t.Errorf("LeftAt = %v", m.LeftAt())
	}

	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	deact, ok := events[0].(membership.DeactivatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want DeactivatedEvent", events[0])
	}
	if deact.Reason != "job change" {
		t.Errorf("event reason = %q", deact.Reason)
	}
}

func TestDeactivate_FromInactive_NoOp(t *testing.T) {

	m := newMembership(t)
	_ = m.PullEvents()
	if err := m.Deactivate("first", testNow); err != nil {
		t.Fatalf("first Deactivate: %v", err)
	}
	_ = m.PullEvents()

	if err := m.Deactivate("repeat", testNow); err != nil {
		t.Fatalf("idempotent Deactivate: %v", err)
	}
	if got := m.PullEvents(); len(got) != 0 {
		t.Errorf("idempotent Deactivate emitted %d events, want 0", len(got))
	}
}

func TestDeactivate_RequiresReason(t *testing.T) {
	m := newMembership(t)
	if err := m.Deactivate("", testNow); err == nil {
		t.Fatal("expected error on empty reason")
	}
}

// ----- Reactivate -----------------------------------------------------------

func TestReactivate_FromInactive_TransitionsToActive(t *testing.T) {

	m := newMembership(t)
	_ = m.PullEvents()
	_ = m.Deactivate("paused", testNow)
	_ = m.PullEvents()

	if err := m.Reactivate(testNow); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if m.Status() != membership.StatusActive {
		t.Errorf("Status = %v, want StatusActive", m.Status())
	}
	if !m.LeftAt().IsZero() {
		t.Errorf("LeftAt should clear on reactivation, got %v", m.LeftAt())
	}

	events := m.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(membership.ReactivatedEvent); !ok {
		t.Errorf("event[0] = %T, want ReactivatedEvent", events[0])
	}
}

func TestReactivate_FromActive_NoOp(t *testing.T) {
	m := newMembership(t)
	_ = m.PullEvents()

	if err := m.Reactivate(testNow); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	if got := m.PullEvents(); len(got) != 0 {
		t.Errorf("idempotent Reactivate emitted %d events, want 0", len(got))
	}
}

// ----- Re-hydration --------------------------------------------------------

func TestUnmarshalFromDB_DoesNotEmitEvents(t *testing.T) {
	t.Parallel()
	m := membership.UnmarshalFromDB(membership.Snapshot{
		ID:       newMembershipID(t),
		PersonID: newPersonID(t),
		TenantID: newTenantID(t),
		Status:   membership.StatusActive,
		JoinedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if m == nil {
		t.Fatal("UnmarshalFromDB returned nil")
	}
	if got := m.PullEvents(); len(got) != 0 {
		t.Errorf("re-hydration emitted %d events, want 0", len(got))
	}
}

// ----- Helpers -------------------------------------------------------------

func newMembership(t *testing.T) *membership.Membership {
	t.Helper()
	m, err := membership.New(newMembershipID(t), newPersonID(t), newTenantID(t), membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("newMembership: %v", err)
	}
	return m
}
