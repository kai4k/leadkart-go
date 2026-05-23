package assignmenthistory_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
)

func TestNew_HappyFirstAssignment(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	e, err := assignmenthistory.New("h-1", "tenant-1", "lead-1", "", "mem-A", "mem-mgr", "initial", at)
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
	e, err := assignmenthistory.New("h-2", "t", "l", "mem-A", "mem-B", "mem-mgr", "rebalance", time.Now().UTC())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.PreviousAssignee() != "mem-A" {
		t.Fatalf("prev: %q", e.PreviousAssignee())
	}
}

func TestNew_Invariants(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	tests := []struct {
		name string
		fn   func() error
	}{
		{name: "missing id", fn: func() error {
			_, err := assignmenthistory.New("", "t", "l", "", "a", "by", "", now)
			return err
		}},
		{name: "missing tenant", fn: func() error {
			_, err := assignmenthistory.New("i", "", "l", "", "a", "by", "", now)
			return err
		}},
		{name: "missing lead", fn: func() error {
			_, err := assignmenthistory.New("i", "t", "", "", "a", "by", "", now)
			return err
		}},
		{name: "missing assignee", fn: func() error {
			_, err := assignmenthistory.New("i", "t", "l", "", "", "by", "", now)
			return err
		}},
		{name: "missing assignedBy", fn: func() error {
			_, err := assignmenthistory.New("i", "t", "l", "", "a", "", "", now)
			return err
		}},
		{name: "reason too long", fn: func() error {
			_, err := assignmenthistory.New("i", "t", "l", "", "a", "by", strings.Repeat("x", 2000), now)
			return err
		}},
		{name: "zero assigned_at", fn: func() error {
			_, err := assignmenthistory.New("i", "t", "l", "", "a", "by", "", time.Time{})
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
	now := time.Now().UTC()
	e := assignmenthistory.UnmarshalFromDB(assignmenthistory.Snapshot{
		ID: "h-z", TenantID: "t", LeadID: "l",
		PreviousAssignee: "p", AssigneeMembershipID: "a", AssignedByMembershipID: "by",
		Reason: "x", AssignedAt: now, CreatedAt: now,
	})
	if e.PreviousAssignee() != "p" || e.AssigneeMembershipID() != "a" {
		t.Fatalf("hydrate: %+v", e)
	}
}
