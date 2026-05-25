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

// ----- New: invariant boundaries -------------------------------------------

func TestNew_RejectsZeroID(t *testing.T) {
	t.Parallel()
	_, err := rolehierarchy.New(
		rolehierarchy.ID(""),
		tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		membership.ID(""),
		"",
		fixedClock(),
	)
	if !errors.Is(err, rolehierarchy.ErrInvalidEdge) {
		t.Fatalf("err = %v, want ErrInvalidEdge", err)
	}
}

func TestNew_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	_, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tenant.ID(""),
		role.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		membership.ID(""),
		"",
		fixedClock(),
	)
	if !errors.Is(err, rolehierarchy.ErrInvalidEdge) {
		t.Fatalf("err = %v, want ErrInvalidEdge", err)
	}
}

func TestNew_RejectsZeroChildRoleID(t *testing.T) {
	t.Parallel()
	_, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		role.ID(""),
		role.ID(ids.NewV7().String()),
		membership.ID(""),
		"",
		fixedClock(),
	)
	if !errors.Is(err, rolehierarchy.ErrInvalidEdge) {
		t.Fatalf("err = %v, want ErrInvalidEdge", err)
	}
}

func TestNew_RejectsZeroParentRoleID(t *testing.T) {
	t.Parallel()
	_, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()),
		role.ID(""),
		membership.ID(""),
		"",
		fixedClock(),
	)
	if !errors.Is(err, rolehierarchy.ErrInvalidEdge) {
		t.Fatalf("err = %v, want ErrInvalidEdge", err)
	}
}

func TestNew_SystemPath_EstablishedByMayBeZero(t *testing.T) {
	t.Parallel()
	// Zero establishedBy permitted — system / migration edges carry no actor.
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
		t.Fatalf("system-path New: %v", err)
	}
	if !e.EstablishedByMembershipID().IsZero() {
		t.Errorf("EstablishedByMembershipID = %v, want zero", e.EstablishedByMembershipID())
	}
}

// ----- Remove: invariant boundaries ----------------------------------------

func TestRemove_SystemPath_RemovedByMayBeZero(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	// Zero removedBy permitted — cascade subscribers carry no actor.
	if err := e.Remove(membership.ID(""), "", fixedClock()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !e.RemovedByMembershipID().IsZero() {
		t.Errorf("RemovedByMembershipID = %v, want zero", e.RemovedByMembershipID())
	}
}

func TestRemove_EmptyReason_Accepted(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	if err := e.Remove(membership.ID(ids.NewV7().String()), "   ", fixedClock()); err != nil {
		t.Fatalf("Remove with whitespace-only reason: %v", err)
	}
	if e.RemovalReason() != "" {
		t.Errorf("RemovalReason = %q, want empty after trim", e.RemovalReason())
	}
}

func TestRemove_RecordsEveryEventField(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	remover := membership.ID(ids.NewV7().String())
	reason := "manager moved to another team"
	at := fixedClock().Add(2 * time.Minute)
	if err := e.Remove(remover, reason, at); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	events := e.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev, ok := events[0].(rolehierarchy.RemovedEvent)
	if !ok {
		t.Fatalf("event type = %T", events[0])
	}
	if ev.ID != e.ID() {
		t.Errorf("ev.ID = %v, want %v", ev.ID, e.ID())
	}
	if ev.TenantID != e.TenantID() {
		t.Errorf("ev.TenantID = %v, want %v", ev.TenantID, e.TenantID())
	}
	if ev.ChildRoleID != e.ChildRoleID() {
		t.Errorf("ev.ChildRoleID = %v, want %v", ev.ChildRoleID, e.ChildRoleID())
	}
	if ev.ParentRoleID != e.ParentRoleID() {
		t.Errorf("ev.ParentRoleID = %v, want %v", ev.ParentRoleID, e.ParentRoleID())
	}
	if ev.RemovedByMembershipID != remover {
		t.Errorf("ev.RemovedByMembershipID = %v, want %v", ev.RemovedByMembershipID, remover)
	}
	if ev.Reason != reason {
		t.Errorf("ev.Reason = %q, want %q", ev.Reason, reason)
	}
	if !ev.At.Equal(at.UTC()) {
		t.Errorf("ev.At = %v, want %v", ev.At, at.UTC())
	}
}

// ----- Post-Remove getters --------------------------------------------------

func TestPostRemove_GettersPopulated(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	_ = e.PullEvents()
	established := e.EstablishedAt()
	estBy := e.EstablishedByMembershipID()
	remover := membership.ID(ids.NewV7().String())
	at := fixedClock().Add(5 * time.Minute)
	reason := "this is a valid removal reason"
	if err := e.Remove(remover, reason, at); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// EstablishedAt + EstablishedBy MUST be untouched by Remove.
	if !e.EstablishedAt().Equal(established) {
		t.Errorf("EstablishedAt mutated by Remove: now %v, want %v", e.EstablishedAt(), established)
	}
	if e.EstablishedByMembershipID() != estBy {
		t.Errorf("EstablishedByMembershipID mutated by Remove: now %v, want %v", e.EstablishedByMembershipID(), estBy)
	}
	if !e.RemovedAt().Equal(at.UTC()) {
		t.Errorf("RemovedAt = %v, want %v", e.RemovedAt(), at.UTC())
	}
	if e.RemovalReason() != reason {
		t.Errorf("RemovalReason = %q, want %q", e.RemovalReason(), reason)
	}
}

// ----- Round-trip + PullEvents drain ---------------------------------------

func TestUnmarshalFromDB_RoundTripsEveryField(t *testing.T) {
	t.Parallel()
	tID := tenant.ID(ids.NewV7().String())
	eID := rolehierarchy.ID(ids.NewV7().String())
	cID := role.ID(ids.NewV7().String())
	pID := role.ID(ids.NewV7().String())
	estBy := membership.ID(ids.NewV7().String())
	remBy := membership.ID(ids.NewV7().String())
	estAt := fixedClock()
	remAt := fixedClock().Add(time.Hour)

	snap := rolehierarchy.Snapshot{
		ID:                        eID,
		TenantID:                  tID,
		ChildRoleID:               cID,
		ParentRoleID:              pID,
		EstablishedAt:             estAt,
		EstablishedByMembershipID: estBy,
		Reason:                    "audit reason snapshot fixture",
		RemovedAt:                 remAt,
		RemovedByMembershipID:     remBy,
		RemovalReason:             "cleanup snapshot fixture",
	}
	e := rolehierarchy.UnmarshalFromDB(snap)
	if e.ID() != eID {
		t.Errorf("ID = %v", e.ID())
	}
	if e.TenantID() != tID {
		t.Errorf("TenantID = %v", e.TenantID())
	}
	if e.ChildRoleID() != cID {
		t.Errorf("ChildRoleID = %v", e.ChildRoleID())
	}
	if e.ParentRoleID() != pID {
		t.Errorf("ParentRoleID = %v", e.ParentRoleID())
	}
	if !e.EstablishedAt().Equal(estAt) {
		t.Errorf("EstablishedAt = %v", e.EstablishedAt())
	}
	if e.EstablishedByMembershipID() != estBy {
		t.Errorf("EstablishedByMembershipID = %v", e.EstablishedByMembershipID())
	}
	if e.Reason() != "audit reason snapshot fixture" {
		t.Errorf("Reason = %q", e.Reason())
	}
	if !e.RemovedAt().Equal(remAt) {
		t.Errorf("RemovedAt = %v", e.RemovedAt())
	}
	if e.RemovedByMembershipID() != remBy {
		t.Errorf("RemovedByMembershipID = %v", e.RemovedByMembershipID())
	}
	if e.RemovalReason() != "cleanup snapshot fixture" {
		t.Errorf("RemovalReason = %q", e.RemovalReason())
	}
	if e.IsActive() {
		t.Error("IsActive after Unmarshal w/ RemovedAt set = true, want false")
	}
	// Unmarshal MUST NOT replay events.
	if evs := e.PullEvents(); len(evs) != 0 {
		t.Errorf("PullEvents after Unmarshal = %d, want 0", len(evs))
	}
}

func TestPullEvents_DrainsAndClears(t *testing.T) {
	t.Parallel()
	e := validNew(t)
	first := e.PullEvents()
	if len(first) != 1 {
		t.Fatalf("first PullEvents = %d, want 1", len(first))
	}
	second := e.PullEvents()
	if second != nil {
		t.Errorf("second PullEvents = %v, want nil", second)
	}
}
