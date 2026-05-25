package command_test

import (
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
	h := command.NewLogCallHandler(leads, calls, fixedTime, newTestCallID)
	out, err := h.Handle(t.Context(), command.LogCallCommand{
		LeadID: id, Outcome: calllog.OutcomeInterested, Notes: "warm prospect", LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.CallID.IsZero() {
		t.Fatal("CallID should be set")
	}
	rows, _ := calls.ListByLead(t.Context(), id)
	if len(rows) != 1 {
		t.Fatalf("calls rows: %d", len(rows))
	}
}

func TestLogCall_NotFound(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	calls := newFakeCallLogs()
	h := command.NewLogCallHandler(leads, calls, fixedTime, newTestCallID)
	_, err := h.Handle(t.Context(), command.LogCallCommand{
		LeadID: "missing", Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
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
	convert := command.NewConvertLeadHandler(leads, fixedTime)
	if err := convert.Handle(t.Context(), command.ConvertLeadCommand{LeadID: id, ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewLogCallHandler(leads, calls, fixedTime, newTestCallID)
	_, err := h.Handle(t.Context(), command.LogCallCommand{
		LeadID: id, Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}
