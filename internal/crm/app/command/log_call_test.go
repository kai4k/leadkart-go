package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestLogCall_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	calls := newFakeCallLogs()
	id := seedLead(t, leads)
	h := command.NewLogCallHandler(leads, calls, fixedTime, newTestCallID)
	out, err := h.Handle(t.Context(), command.LogCallCommand{
		TenantID: testTenantID, LeadID: id, Outcome: calllog.OutcomeInterested, Notes: "warm prospect", LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.CallID.IsZero() {
		t.Fatal("CallID should be set")
	}
	rows, _ := calls.ListByLead(t.Context(), testTenantID, id)
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
		TenantID: testTenantID, LeadID: "missing", Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
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
	if err := convert.Handle(t.Context(), command.ConvertLeadCommand{TenantID: testTenantID, LeadID: id, ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewLogCallHandler(leads, calls, fixedTime, newTestCallID)
	_, err := h.Handle(t.Context(), command.LogCallCommand{
		TenantID: testTenantID, LeadID: id, Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}

// ----- Input validation -----------------------------------------------------

func TestLogCall_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	h := command.NewLogCallHandler(newFakeLeads(), newFakeCallLogs(), fixedTime, newTestCallID)
	_, err := h.Handle(t.Context(), command.LogCallCommand{
		TenantID: tenant.ID(""), LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"),
		Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
	})
	if err == nil {
		t.Fatal("want input-validation error on zero TenantID")
	}
}

func TestLogCall_RejectsZeroLeadID(t *testing.T) {
	t.Parallel()
	h := command.NewLogCallHandler(newFakeLeads(), newFakeCallLogs(), fixedTime, newTestCallID)
	_, err := h.Handle(t.Context(), command.LogCallCommand{
		TenantID: testTenantID, LeadID: crmlead.ID(""),
		Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
	})
	if err == nil {
		t.Fatal("want input-validation error on zero LeadID")
	}
}

func TestLogCall_RejectsEmptyLoggedByMembershipID(t *testing.T) {
	t.Parallel()
	h := command.NewLogCallHandler(newFakeLeads(), newFakeCallLogs(), fixedTime, newTestCallID)
	_, err := h.Handle(t.Context(), command.LogCallCommand{
		TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"),
		Outcome: calllog.OutcomeConnected, LoggedByMembershipID: "",
	})
	if err == nil {
		t.Fatal("want input-validation error on empty LoggedByMembershipID")
	}
}

// ----- Factory + persist wrapping ------------------------------------------

func TestLogCall_FactoryRejection_Wrapped(t *testing.T) {
	t.Parallel()
	// Invalid outcome string lands in calllog.New as ErrInvalid (the
	// catalogue check). Handler must wrap that into "crm log_call:
	// factory: ..." rather than propagating raw — verifies the factory
	// error-wrap branch.
	leads := newFakeLeads()
	calls := newFakeCallLogs()
	id := seedLead(t, leads)
	h := command.NewLogCallHandler(leads, calls, fixedTime, newTestCallID)
	_, err := h.Handle(t.Context(), command.LogCallCommand{
		TenantID: testTenantID, LeadID: id,
		Outcome:              calllog.Outcome("not-in-catalogue"),
		LoggedByMembershipID: "01923400-0000-7000-8000-cccccccc0004",
	})
	if err == nil {
		t.Fatal("want factory error, got nil")
	}
	if !errors.Is(err, calllog.ErrInvalid) {
		t.Fatalf("want calllog.ErrInvalid wrapped, got %v", err)
	}
}
