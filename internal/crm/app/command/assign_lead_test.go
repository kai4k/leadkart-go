package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
)

func fixedTime() time.Time {
	return time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
}

func TestAssignLead_FirstAssignment(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime)
	out, err := h.Handle(context.Background(), command.AssignLeadCommand{
		LeadID: id, AssigneeMembershipID: "mem-A", AssignedByMembershipID: "mem-manager",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.AssignmentID == "" {
		t.Fatal("AssignmentID should be set")
	}
	// Lead's mirrored assignee should match.
	got, _ := leads.GetByID(context.Background(), id)
	if got.AssigneeMembershipID() != "mem-A" {
		t.Fatalf("lead.assignee: %q", got.AssigneeMembershipID())
	}
	// History row written.
	rows, _ := history.ListByLead(context.Background(), id)
	if len(rows) != 1 {
		t.Fatalf("history rows: %d", len(rows))
	}
	if rows[0].AssigneeMembershipID() != "mem-A" || rows[0].PreviousAssignee() != "" {
		t.Fatalf("history row fields: %+v", rows[0])
	}
}

func TestAssignLead_IdempotentSelfAssignWritesNoHistory(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime)
	if _, err := h.Handle(context.Background(), command.AssignLeadCommand{
		LeadID: id, AssigneeMembershipID: "mem-A", AssignedByMembershipID: "mem-manager",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Self-assign — must NOT write a new history row.
	out, err := h.Handle(context.Background(), command.AssignLeadCommand{
		LeadID: id, AssigneeMembershipID: "mem-A", AssignedByMembershipID: "mem-manager",
	})
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if out.AssignmentID != "" {
		t.Fatalf("self-assign should return zero AssignmentID, got %q", out.AssignmentID)
	}
	rows, _ := history.ListByLead(context.Background(), id)
	if len(rows) != 1 {
		t.Fatalf("history rows after self-assign: %d (want 1)", len(rows))
	}
}

func TestAssignLead_Reassignment(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	id := seedLead(t, leads)
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime)
	if _, err := h.Handle(context.Background(), command.AssignLeadCommand{
		LeadID: id, AssigneeMembershipID: "mem-A", AssignedByMembershipID: "mgr",
	}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := h.Handle(context.Background(), command.AssignLeadCommand{
		LeadID: id, AssigneeMembershipID: "mem-B", AssignedByMembershipID: "mgr", Reason: "rebalance",
	}); err != nil {
		t.Fatalf("second: %v", err)
	}
	rows, _ := history.ListByLead(context.Background(), id)
	if len(rows) != 2 {
		t.Fatalf("history rows: %d", len(rows))
	}
	// Find the second-write row + assert previous_assignee is mem-A.
	var reassign *struct{ prev, assignee, reason string }
	for _, r := range rows {
		if r.AssigneeMembershipID() == "mem-B" {
			reassign = &struct{ prev, assignee, reason string }{r.PreviousAssignee(), r.AssigneeMembershipID(), r.Reason()}
		}
	}
	if reassign == nil || reassign.prev != "mem-A" || reassign.reason != "rebalance" {
		t.Fatalf("reassign row: %+v", reassign)
	}
}

func TestAssignLead_NotFound(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	history := newFakeHistory()
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime)
	_, err := h.Handle(context.Background(), command.AssignLeadCommand{
		LeadID: "nope", AssigneeMembershipID: "mem-A", AssignedByMembershipID: "mgr",
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
	convert := command.NewConvertLeadHandler(leads)
	if err := convert.Handle(context.Background(), command.ConvertLeadCommand{LeadID: id, ConvertedByMembershipID: "mem"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewAssignLeadHandler(leads, history, fakeUoW{}, fixedTime)
	_, err := h.Handle(context.Background(), command.AssignLeadCommand{
		LeadID: id, AssigneeMembershipID: "mem-A", AssignedByMembershipID: "mgr",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}
