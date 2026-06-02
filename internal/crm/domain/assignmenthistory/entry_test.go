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

// Real UUIDs — every domain ID must parse as a UUID at construction
// (H6 reviewer rule, mirrored from crmlead/calllog).
const (
	histID     = "01923400-0000-7000-8000-aaaaaaaa0001"
	tenantID   = "01923400-0000-7000-8000-bbbbbbbb0001"
	leadID     = "01923400-0000-7000-8000-cccccccc0001"
	memA       = "01923400-0000-7000-8000-dddddddd0001"
	memB       = "01923400-0000-7000-8000-dddddddd0002"
	memManager = "01923400-0000-7000-8000-eeeeeeee0001"
)

func TestNew_HappyFirstAssignment(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	e, err := assignmenthistory.New(histID, tenant.ID(tenantID), leadID, "", memA, memManager, "initial", at, at)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.PreviousAssignee() != "" || e.AssigneeMembershipID() != memA || e.Reason() != "initial" {
		t.Fatalf("fields: %+v", e)
	}
	if !e.AssignedAt().Equal(at) {
		t.Fatalf("AssignedAt: %v", e.AssignedAt())
	}
}

func TestNew_HappyReassignment(t *testing.T) {
	t.Parallel()
	e, err := assignmenthistory.New(histID, tenant.ID(tenantID), leadID, memA, memB, memManager, "rebalance", fixedNow, fixedNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.PreviousAssignee() != memA {
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
			_, err := assignmenthistory.New("", tenant.ID(tenantID), leadID, "", memA, memManager, "", now, now)
			return err
		}},
		{name: "non-uuid id", fn: func() error {
			_, err := assignmenthistory.New("not-a-uuid", tenant.ID(tenantID), leadID, "", memA, memManager, "", now, now)
			return err
		}},
		{name: "missing tenant", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID(""), leadID, "", memA, memManager, "", now, now)
			return err
		}},
		{name: "non-uuid tenant", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID("t"), leadID, "", memA, memManager, "", now, now)
			return err
		}},
		{name: "missing lead", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID(tenantID), "", "", memA, memManager, "", now, now)
			return err
		}},
		{name: "missing assignee", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID(tenantID), leadID, "", "", memManager, "", now, now)
			return err
		}},
		{name: "missing assignedBy", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID(tenantID), leadID, "", memA, "", "", now, now)
			return err
		}},
		{name: "non-uuid previous assignee", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID(tenantID), leadID, "garbage", memA, memManager, "", now, now)
			return err
		}},
		{name: "reason too long", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID(tenantID), leadID, "", memA, memManager, strings.Repeat("x", 2000), now, now)
			return err
		}},
		{name: "zero assigned_at", fn: func() error {
			_, err := assignmenthistory.New(histID, tenant.ID(tenantID), leadID, "", memA, memManager, "", time.Time{}, time.Time{})
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
	// UnmarshalFromDB skips validation — DB rows are already trusted, so
	// short non-UUID fixtures are fine here.
	e := assignmenthistory.UnmarshalFromDB(assignmenthistory.Snapshot{
		ID: "h-z", TenantID: tenant.ID("t"), LeadID: "l",
		PreviousAssignee: "p", AssigneeMembershipID: "a", AssignedByMembershipID: "by",
		Reason: "x", AssignedAt: now, CreatedAt: now,
	})
	if e.PreviousAssignee() != "p" || e.AssigneeMembershipID() != "a" {
		t.Fatalf("hydrate: %+v", e)
	}
}
