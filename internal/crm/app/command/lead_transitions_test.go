package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedSeed is the deterministic clock used by every seedLead caller
// across the command-test suite. Stable wall-clock keeps the emitted-
// event timestamps comparable between runs.
var fixedSeed = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

// seedLead creates a fresh CrmLead inside the fake repository + returns
// its ID â€” used by every transition test. The lead carries
// [testTenantID] so per-aggregate fake tenant filtering aligns with
// the command's TenantID payload.
func seedLead(t *testing.T, r *crmleadtest.FakeRepository) crmlead.ID {
	t.Helper()
	l, err := crmlead.New(
		crmlead.ID(ids.NewV7().String()),
		testTenantID,
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
		TenantID: testTenantID, LeadID: id, NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a", Reason: "first call",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), testTenantID, id)
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
		TenantID: testTenantID, LeadID: id, NewStage: crmlead.StageNegotiation, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
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
		TenantID: testTenantID, LeadID: "nope", NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
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
	if err := convert.Handle(t.Context(), command.ConvertLeadCommand{TenantID: testTenantID, LeadID: id, ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"}); err != nil {
		t.Fatalf("convert: %v", err)
	}
	h := command.NewChangeLeadStageHandler(leads, fixedTime)
	err := h.Handle(t.Context(), command.ChangeLeadStageCommand{
		TenantID: testTenantID, LeadID: id, NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	})
	if !errors.Is(err, command.ErrLeadTerminal) {
		t.Fatalf("want ErrLeadTerminal, got %v", err)
	}
}

// ----- ChangeStage: input validation table ----------------------------------

func TestChangeStage_InputValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  command.ChangeLeadStageCommand
	}{
		{
			"zero tenant id",
			command.ChangeLeadStageCommand{TenantID: tenant.ID(""), LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"},
		},
		{
			"zero lead id",
			command.ChangeLeadStageCommand{TenantID: testTenantID, LeadID: crmlead.ID(""), NewStage: crmlead.StageContacted, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"},
		},
		{
			"empty changed-by membership id",
			command.ChangeLeadStageCommand{TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), NewStage: crmlead.StageContacted, ChangedByMembershipID: ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewChangeLeadStageHandler(newFakeLeads(), fixedTime)
			err := h.Handle(t.Context(), tc.cmd)
			if err == nil {
				t.Fatal("want input-validation error, got nil")
			}
		})
	}
}

// ----- ChangeStage: no-op same-stage branch ---------------------------------

func TestChangeStage_NoopSameStage_DoesNotEmitEvent(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	// Drain the LeadCreated event from seed.
	leads.EmittedEventsByLead[id] = nil

	h := command.NewChangeLeadStageHandler(leads, fixedTime)
	// Brand-new lead is in StageNew → passing StageNew is the no-op branch.
	if err := h.Handle(t.Context(), command.ChangeLeadStageCommand{
		TenantID: testTenantID, LeadID: id, NewStage: crmlead.StageNew, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	}); err != nil {
		t.Fatalf("no-op same-stage: %v", err)
	}
	if evs := leads.EmittedEventsByLead[id]; len(evs) != 0 {
		t.Errorf("no-op same-stage should emit 0 events, got %d: %+v", len(evs), evs)
	}
	got, _ := leads.GetByID(t.Context(), testTenantID, id)
	if got.Stage() != crmlead.StageNew {
		t.Errorf("stage = %s, want StageNew (unchanged)", got.Stage())
	}
}

func TestChangeTemperature_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewChangeLeadTemperatureHandler(leads, fixedTime)
	if err := h.Handle(t.Context(), command.ChangeLeadTemperatureCommand{
		TenantID: testTenantID, LeadID: id, NewTemperature: crmlead.TemperatureHot, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), testTenantID, id)
	if got.Temperature() != crmlead.TemperatureHot {
		t.Fatalf("temperature: %s", got.Temperature())
	}
}

// ----- ChangeTemperature: input validation table ---------------------------

func TestChangeTemperature_InputValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  command.ChangeLeadTemperatureCommand
	}{
		{
			"zero tenant id",
			command.ChangeLeadTemperatureCommand{TenantID: tenant.ID(""), LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), NewTemperature: crmlead.TemperatureHot, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"},
		},
		{
			"zero lead id",
			command.ChangeLeadTemperatureCommand{TenantID: testTenantID, LeadID: crmlead.ID(""), NewTemperature: crmlead.TemperatureHot, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"},
		},
		{
			"empty changed-by membership id",
			command.ChangeLeadTemperatureCommand{TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), NewTemperature: crmlead.TemperatureHot, ChangedByMembershipID: ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewChangeLeadTemperatureHandler(newFakeLeads(), fixedTime)
			err := h.Handle(t.Context(), tc.cmd)
			if err == nil {
				t.Fatal("want input-validation error, got nil")
			}
		})
	}
}

// ----- ChangeTemperature: no-op same-temp branch ---------------------------

func TestChangeTemperature_NoopSameTemp_DoesNotEmitEvent(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	leads.EmittedEventsByLead[id] = nil

	h := command.NewChangeLeadTemperatureHandler(leads, fixedTime)
	// Brand-new lead is TemperatureWarm by default → same-temp no-op.
	if err := h.Handle(t.Context(), command.ChangeLeadTemperatureCommand{
		TenantID: testTenantID, LeadID: id, NewTemperature: crmlead.TemperatureWarm, ChangedByMembershipID: "01923400-0000-7000-8000-cccccccc000a",
	}); err != nil {
		t.Fatalf("no-op same-temp: %v", err)
	}
	if evs := leads.EmittedEventsByLead[id]; len(evs) != 0 {
		t.Errorf("no-op same-temp should emit 0 events, got %d: %+v", len(evs), evs)
	}
}

func TestConvertLead_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewConvertLeadHandler(leads, fixedTime)
	if err := h.Handle(t.Context(), command.ConvertLeadCommand{TenantID: testTenantID, LeadID: id, ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), testTenantID, id)
	if got.Stage() != crmlead.StageConverted {
		t.Fatalf("stage: %s", got.Stage())
	}
}

// ----- ConvertLead: input validation table ---------------------------------

func TestConvertLead_InputValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  command.ConvertLeadCommand
	}{
		{
			"zero tenant id",
			command.ConvertLeadCommand{TenantID: tenant.ID(""), LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"},
		},
		{
			"zero lead id",
			command.ConvertLeadCommand{TenantID: testTenantID, LeadID: crmlead.ID(""), ConvertedByMembershipID: "01923400-0000-7000-8000-cccccccc000a"},
		},
		{
			"empty converted-by membership id",
			command.ConvertLeadCommand{TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), ConvertedByMembershipID: ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewConvertLeadHandler(newFakeLeads(), fixedTime)
			err := h.Handle(t.Context(), tc.cmd)
			if err == nil {
				t.Fatal("want input-validation error, got nil")
			}
		})
	}
}

func TestLoseLead_RequiresReason(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewLoseLeadHandler(leads, fixedTime)
	err := h.Handle(t.Context(), command.LoseLeadCommand{TenantID: testTenantID, LeadID: id, LostByMembershipID: "01923400-0000-7000-8000-cccccccc000a"})
	if err == nil {
		t.Fatal("want error on missing reason")
	}
}

func TestLoseLead_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	id := seedLead(t, leads)
	h := command.NewLoseLeadHandler(leads, fixedTime)
	if err := h.Handle(t.Context(), command.LoseLeadCommand{TenantID: testTenantID, LeadID: id, LostByMembershipID: "01923400-0000-7000-8000-cccccccc000a", Reason: "no budget"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := leads.GetByID(t.Context(), testTenantID, id)
	if got.Stage() != crmlead.StageLost {
		t.Fatalf("stage: %s", got.Stage())
	}
}

// ----- LoseLead: input validation table ------------------------------------

func TestLoseLead_InputValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  command.LoseLeadCommand
	}{
		{
			"zero tenant id",
			command.LoseLeadCommand{TenantID: tenant.ID(""), LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), LostByMembershipID: "01923400-0000-7000-8000-cccccccc000a", Reason: "no budget"},
		},
		{
			"zero lead id",
			command.LoseLeadCommand{TenantID: testTenantID, LeadID: crmlead.ID(""), LostByMembershipID: "01923400-0000-7000-8000-cccccccc000a", Reason: "no budget"},
		},
		{
			"empty lost-by membership id",
			command.LoseLeadCommand{TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-cccccccc0001"), LostByMembershipID: "", Reason: "no budget"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewLoseLeadHandler(newFakeLeads(), fixedTime)
			err := h.Handle(t.Context(), tc.cmd)
			if err == nil {
				t.Fatal("want input-validation error, got nil")
			}
		})
	}
}
