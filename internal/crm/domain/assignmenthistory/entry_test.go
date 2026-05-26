package assignmenthistory_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedNow pins the wall-clock so emitted-event timestamps are
// deterministic across the table tests (Khorikov §8 — pin time).
var fixedNow = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

func TestNew_HappyFirstAssignment(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	e, err := assignmenthistory.New("h-1", tenant.ID("tenant-1"), "lead-1", "", "mem-A", "mem-mgr", "initial", at, at)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.PreviousAssignee() != "" || e.AssigneeMembershipID() != "mem-A" || e.Reason() != "initial" {
		t.Fatalf("fields: %+v", e)
	}
	if !e.AssignedAt().Equal(at) {
		t.Fatalf("AssignedAt: %v", e.AssignedAt())
	}
}

func TestNew_HappyReassignment(t *testing.T) {
	t.Parallel()
	e, err := assignmenthistory.New("h-2", tenant.ID("t"), "l", "mem-A", "mem-B", "mem-mgr", "rebalance", fixedNow, fixedNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.PreviousAssignee() != "mem-A" {
		t.Fatalf("prev: %q", e.PreviousAssignee())
	}
}

func TestNew_Invariants(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tests := []struct {
		name string
		fn   func() error
	}{
		{name: "missing id", fn: func() error {
			_, err := assignmenthistory.New("", tenant.ID("t"), "l", "", "a", "by", "", now, now)
			return err
		}},
		{name: "missing tenant", fn: func() error {
			_, err := assignmenthistory.New("i", tenant.ID(""), "l", "", "a", "by", "", now, now)
			return err
		}},
		{name: "missing lead", fn: func() error {
			_, err := assignmenthistory.New("i", tenant.ID("t"), "", "", "a", "by", "", now, now)
			return err
		}},
		{name: "missing assignee", fn: func() error {
			_, err := assignmenthistory.New("i", tenant.ID("t"), "l", "", "", "by", "", now, now)
			return err
		}},
		{name: "missing assignedBy", fn: func() error {
			_, err := assignmenthistory.New("i", tenant.ID("t"), "l", "", "a", "", "", now, now)
			return err
		}},
		{name: "reason too long", fn: func() error {
			_, err := assignmenthistory.New("i", tenant.ID("t"), "l", "", "a", "by", strings.Repeat("x", 2000), now, now)
			return err
		}},
		{name: "zero assigned_at", fn: func() error {
			_, err := assignmenthistory.New("i", tenant.ID("t"), "l", "", "a", "by", "", time.Time{}, time.Time{})
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.fn()
			if !errors.Is(err, assignmenthistory.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestUnmarshalFromDB(t *testing.T) {
	t.Parallel()
	now := fixedNow
	e := assignmenthistory.UnmarshalFromDB(assignmenthistory.Snapshot{
		ID: "h-z", TenantID: tenant.ID("t"), LeadID: "l",
		PreviousAssignee: "p", AssigneeMembershipID: "a", AssignedByMembershipID: "by",
		Reason: "x", AssignedAt: now, CreatedAt: now,
	})
	if e.PreviousAssignee() != "p" || e.AssigneeMembershipID() != "a" {
		t.Fatalf("hydrate: %+v", e)
	}
}
