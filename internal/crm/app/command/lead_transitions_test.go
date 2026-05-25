package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
)

// fixedSeed is the deterministic clock used by every seedLead caller
// across the command-test suite. Stable wall-clock keeps the emitted-
// event timestamps comparable between runs.
var fixedSeed = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

// seedLead creates a fresh CrmLead inside the fake repository + returns
// its ID â€” used by every transition test.
func seedLead(t *testing.T, r *crmleadtest.FakeRepository) crmlead.ID {
	t.Helper()
	l, err := crmlead.New(
		crmlead.ID(ids.NewV7().String()),
		"01923400-0000-7000-8000-000000000001",
		crmlead.Profile{ContactName: "X", PhoneE164: "+919812345678"},
		"01923400-0000-7000-8000-cccccccc0001",
		fixedSeed,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := r.Add(t.Context(), l); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	return l.ID()
}

func TestChangeStage_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewChangeLeadStageHandler(leads, fixedTime)
	if err := h.Handle(t.Context(), command.ChangeLeadStageCommand{
		LeadID: id, NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a", Reason: "first call",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), id)
	if got.Stage() != crmlead.StageContacted {
		t.Fatalf("stage: %s", got.Stage())
	}
}

func TestChangeStage_RejectsSkip(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewChangeLeadStageHandler(leads, fixedTime)
	err := h.Handle(t.Context(), command.ChangeLeadStageCommand{
		LeadID: id, NewStage: crmlead.StageNegotiation, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	})
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestChangeStage_NotFound(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := command.NewChangeLeadStageHandler(leads, fixedTime)
	err := h.Handle(t.Context(), command.ChangeLeadStageCommand{
		LeadID: "nope", NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	})
	if !errors.Is(err, command.ErrLeadNotFound) {
		t.Fatalf("want ErrLeadNotFound, got %v", err)
	}
}

func TestChangeStage_Terminal(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	convert := command.NewConvertLeadHandler(leads, fixedTime)
	if err := convert.Handle(t.Context(), command.ConvertLeadCommand{LeadID: id, ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewChangeLeadStageHandler(leads, fixedTime)
	err := h.Handle(t.Context(), command.ChangeLeadStageCommand{
		LeadID: id, NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}

func TestChangeTemperature_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewChangeLeadTemperatureHandler(leads, fixedTime)
	if err := h.Handle(t.Context(), command.ChangeLeadTemperatureCommand{
		LeadID: id, NewTemperature: crmlead.TemperatureHot, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), id)
	if got.Temperature() != crmlead.TemperatureHot {
		t.Fatalf("temperature: %s", got.Temperature())
	}
}

func TestConvertLead_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewConvertLeadHandler(leads, fixedTime)
	if err := h.Handle(t.Context(), command.ConvertLeadCommand{LeadID: id, ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), id)
	if got.Stage() != crmlead.StageConverted {
		t.Fatalf("stage: %s", got.Stage())
	}
}

func TestLoseLead_RequiresReason(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewLoseLeadHandler(leads, fixedTime)
	err := h.Handle(t.Context(), command.LoseLeadCommand{LeadID: id, LostByMembershipID: "01923400-0000-7000-8000-cccccccc000a"})
	if err == nil {
		t.Fatal("want error on missing reason")
	}
}

func TestLoseLead_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewLoseLeadHandler(leads, fixedTime)
	if err := h.Handle(t.Context(), command.LoseLeadCommand{LeadID: id, LostByMembershipID: "01923400-0000-7000-8000-cccccccc000a", Reason: "no budget"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), id)
	if got.Stage() != crmlead.StageLost {
		t.Fatalf("stage: %s", got.Stage())
	}
}
