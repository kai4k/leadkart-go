package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func fixedTime() time.Time {
	return time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
}

func TestAssignLead_FirstAssignment(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime, newTestHistoryID)
	out, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: id, AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.AssignmentID == "" {
		t.Fatal("AssignmentID should be set")
	}
	// Lead's mirrored assignee should match.
	got, _ := leads.GetByID(t.Context(), testTenantID, id)
	if got.AssigneeMembershipID() != "01923400-0000-7000-8000-cccccccc0004" {
		t.Fatalf("lead.assignee: %q", got.AssigneeMembershipID())
	}
	// History row written.
	rows, _ := history.ListByLead(t.Context(), testTenantID, id)
	if len(rows) != 1 {
		t.Fatalf("history rows: %d", len(rows))
	}
	if rows[0].AssigneeMembershipID() != "01923400-0000-7000-8000-cccccccc0004" || rows[0].PreviousAssignee() != "" {
		t.Fatalf("history row fields: %+v", rows[0])
	}
}

func TestAssignLead_IdempotentSelfAssignWritesNoHistory(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime, newTestHistoryID)
	if _, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: id, AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Self-assign — must NOT write a new history row.
	out, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: id, AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006",
	})
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if out.AssignmentID != "" {
		t.Fatalf("self-assign should return zero AssignmentID, got %q", out.AssignmentID)
	}
	rows, _ := history.ListByLead(t.Context(), testTenantID, id)
	if len(rows) != 1 {
		t.Fatalf("history rows after self-assign: %d (want 1)", len(rows))
	}
}

func TestAssignLead_Reassignment(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime, newTestHistoryID)
	if _, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: id, AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: id, AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0005", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006", Reason: "rebalance",
	}); err != nil {
		t.Fatalf("second: %v", err)
	}
	rows, _ := history.ListByLead(t.Context(), testTenantID, id)
	if len(rows) != 2 {
		t.Fatalf("history rows: %d", len(rows))
	}
	// Find the second-write row + assert previous_assignee is mem-A.
	var reassign *struct{ prev, assignee, reason string }
	for _, r := range rows {
		if r.AssigneeMembershipID() == "01923400-0000-7000-8000-cccccccc0005" {
			reassign = &struct{ prev, assignee, reason string }{r.PreviousAssignee(), r.AssigneeMembershipID(), r.Reason()}
		}
	}
	if reassign == nil || reassign.prev != "01923400-0000-7000-8000-cccccccc0004" || reassign.reason != "rebalance" {
		t.Fatalf("reassign row: %+v", reassign)
	}
}

func TestAssignLead_NotFound(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime, newTestHistoryID)
	_, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: "nope", AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006",
	})
	if !errors.Is(err, command.ErrLeadNotFound) {
		t.Fatalf("want ErrLeadNotFound, got %v", err)
	}
}

func TestAssignLead_Terminal(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	convert := command.NewConvertLeadHandler(leads, fixedTime)
	if err := convert.Handle(t.Context(), command.ConvertLeadCommand{TenantID: testTenantID, LeadID: id, ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime, newTestHistoryID)
	_, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: id, AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}

// ----- Input validation -----------------------------------------------------

func TestAssignLead_InputValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  command.AssignLeadCommand
	}{
		{
			"zero tenant id",
			command.AssignLeadCommand{TenantID: tenant.ID(""), LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006"},
		},
		{
			"zero lead id",
			command.AssignLeadCommand{TenantID: testTenantID, LeadID: crmlead.ID(""), AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006"},
		},
		{
			"empty assignee membership id",
			command.AssignLeadCommand{TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), AssigneeMembershipID: "", AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006"},
		},
		{
			"empty assigned-by membership id",
			command.AssignLeadCommand{TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), AssigneeMembershipID: "01923400-0000-7000-8000-cccccccc0004", AssignedByMembershipID: ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewAssignLeadHandler(newFakeLeads(), newFakeHistory(), fakeUoW{}, fixedTime, newTestHistoryID)
			_, err := h.Handle(t.Context(), tc.cmd)
			if err == nil {
				t.Fatal("want input-validation error, got nil")
			}
		})
	}
}

// ----- History factory rejection wrapping ----------------------------------

// historyIDFactoryEmpty returns the zero-value assignmenthistory.ID. The
// downstream assignmenthistory.New ctor MUST reject this with ErrInvalid;
// the handler wraps that into "crm assign: history factory: ...".
func historyIDFactoryEmpty() assignmenthistory.ID { return assignmenthistory.ID("") }

func TestAssignLead_HistoryFactoryRejection_Wrapped(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	// Inject the empty-ID factory so the assignmenthistory.New ctor fires
	// its "id required" branch. Verifies the handler's "history factory"
	// wrap path is exercised (this branch was previously untested).
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime, historyIDFactoryEmpty)
	_, err := h.Handle(t.Context(), command.AssignLeadCommand{
		TenantID: testTenantID, LeadID: id,
		AssigneeMembershipID:   "01923400-0000-7000-8000-cccccccc0004",
		AssignedByMembershipID: "01923400-0000-7000-8000-cccccccc0006",
	})
	if err == nil {
		t.Fatal("want history-factory error, got nil")
	}
	if !errors.Is(err, assignmenthistory.ErrInvalid) {
		t.Fatalf("want assignmenthistory.ErrInvalid wrapped, got %v", err)
	}
}
