package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
)

func TestLogCall_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	calls := newFakeCallLogs()
	id := seedLead(t, leads)
	h := command.NewLogCallHandler(leads, calls, fixedTime)
	out, err := h.Handle(context.Background(), command.LogCallCommand{
		LeadID: id, Outcome: calllog.OutcomeInterested, Notes: "warm prospect", LoggedByMembershipID: "mem-A",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.CallID.IsZero() {
		t.Fatal("CallID should be set")
	}
	rows, _ := calls.ListByLead(context.Background(), id)
	if len(rows) != 1 {
		t.Fatalf("calls rows: %d", len(rows))
	}
}

func TestLogCall_NotFound(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	calls := newFakeCallLogs()
	h := command.NewLogCallHandler(leads, calls, fixedTime)
	_, err := h.Handle(context.Background(), command.LogCallCommand{
		LeadID: "missing", Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "mem-A",
	})
	if !errors.Is(err, command.ErrLeadNotFound) {
		t.Fatalf("want ErrLeadNotFound, got %v", err)
	}
}

func TestLogCall_RefusesTerminalLead(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	calls := newFakeCallLogs()
	id := seedLead(t, leads)
	convert := command.NewConvertLeadHandler(leads)
	if err := convert.Handle(context.Background(), command.ConvertLeadCommand{LeadID: id, ConvertedByMembershipID: "mem"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewLogCallHandler(leads, calls, fixedTime)
	_, err := h.Handle(context.Background(), command.LogCallCommand{
		LeadID: id, Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "mem-A",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}
