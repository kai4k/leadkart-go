package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// seedLead creates a fresh CrmLead inside the fake repository + returns
// its ID — used by every transition test.
func seedLead(t *testing.T, r *fakeLeads) crmlead.ID {
	t.Helper()
	clock.Set(time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)
	l, err := crmlead.New(
		crmlead.ID(ids.NewV7().String()),
		"01923400-0000-7000-8000-000000000001",
		crmlead.Profile{ContactName: "X", PhoneE164: "+919812345678"},
		"mem-creator",
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := r.Add(context.Background(), l); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	return l.ID()
}

func TestChangeStage_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewChangeLeadStageHandler(leads)
	if err := h.Handle(context.Background(), command.ChangeLeadStageCommand{
		LeadID: id, NewStage: crmlead.StageContacted, ChangedByMembershipID: "mem", Reason: "first call",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(context.Background(), id)
	if got.Stage() != crmlead.StageContacted {
		t.Fatalf("stage: %s", got.Stage())
	}
}

func TestChangeStage_RejectsSkip(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewChangeLeadStageHandler(leads)
	err := h.Handle(context.Background(), command.ChangeLeadStageCommand{
		LeadID: id, NewStage: crmlead.StageNegotiation, ChangedByMembershipID: "mem",
	})
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestChangeStage_NotFound(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := command.NewChangeLeadStageHandler(leads)
	err := h.Handle(context.Background(), command.ChangeLeadStageCommand{
		LeadID: "nope", NewStage: crmlead.StageContacted, ChangedByMembershipID: "mem",
	})
	if !errors.Is(err, command.ErrLeadNotFound) {
		t.Fatalf("want ErrLeadNotFound, got %v", err)
	}
}

func TestChangeStage_Terminal(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	convert := command.NewConvertLeadHandler(leads)
	if err := convert.Handle(context.Background(), command.ConvertLeadCommand{LeadID: id, ConvertedByMembershipID: "mem"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewChangeLeadStageHandler(leads)
	err := h.Handle(context.Background(), command.ChangeLeadStageCommand{
		LeadID: id, NewStage: crmlead.StageContacted, ChangedByMembershipID: "mem",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}

func TestChangeTemperature_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewChangeLeadTemperatureHandler(leads)
	if err := h.Handle(context.Background(), command.ChangeLeadTemperatureCommand{
		LeadID: id, NewTemperature: crmlead.TemperatureHot, ChangedByMembershipID: "mem",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(context.Background(), id)
	if got.Temperature() != crmlead.TemperatureHot {
		t.Fatalf("temperature: %s", got.Temperature())
	}
}

func TestConvertLead_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewConvertLeadHandler(leads)
	if err := h.Handle(context.Background(), command.ConvertLeadCommand{LeadID: id, ConvertedByMembershipID: "mem"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(context.Background(), id)
	if got.Stage() != crmlead.StageConverted {
		t.Fatalf("stage: %s", got.Stage())
	}
}

func TestLoseLead_RequiresReason(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewLoseLeadHandler(leads)
	err := h.Handle(context.Background(), command.LoseLeadCommand{LeadID: id, LostByMembershipID: "mem"})
	if err == nil {
		t.Fatal("want error on missing reason")
	}
}

func TestLoseLead_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewLoseLeadHandler(leads)
	if err := h.Handle(context.Background(), command.LoseLeadCommand{LeadID: id, LostByMembershipID: "mem", Reason: "no budget"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(context.Background(), id)
	if got.Stage() != crmlead.StageLost {
		t.Fatalf("stage: %s", got.Stage())
	}
}
