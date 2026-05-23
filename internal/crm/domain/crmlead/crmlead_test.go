package crmlead_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// fixedNow is the deterministic clock used across the table tests so
// the emitted-event timestamps are predictable.
var fixedNow = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

func withFixedClock(t *testing.T) {
	t.Helper()
	clock.Set(fixedNow)
	t.Cleanup(clock.Reset)
}

func validProfile() crmlead.Profile {
	return crmlead.Profile{
		ContactName:    "Sharma Medical Store",
		PhoneE164:      "+919876543210",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		Pincode:        "411001",
		BusinessType:   "PCD",
		MedicineSystem: "Allopathic",
		OrderValue:     "Upto25000",
		BuyTimeline:    "WithinWeek",
		HasDrugLicence: true,
		HasGst:         true,
		ProductRanges:  []string{"Antibiotics", "Cardiac"},
		DosageForms:    []string{"Tablet", "Capsule"},
		Extra: crmlead.ExtraProfile{
			Street:    "MG Road 12",
			GstNumber: "27ABCDE1234F1Z5",
			HasPan:    true,
			PanNumber: "ABCDE1234F",
			Email:     "owner@sharma.example",
		},
	}
}

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	withFixedClock(t)

	l, err := crmlead.New("lead-1", "tenant-1", validProfile(), "mem-creator")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if l.ID() != "lead-1" {
		t.Fatalf("ID: %q", l.ID())
	}
	if l.Stage() != crmlead.StageNew {
		t.Fatalf("Stage: %q", l.Stage())
	}
	if l.Temperature() != crmlead.TemperatureWarm {
		t.Fatalf("Temperature: %q", l.Temperature())
	}
	if !l.CreatedAt().Equal(fixedNow) {
		t.Fatalf("CreatedAt: %v", l.CreatedAt())
	}

	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: want 1 got %d", len(evs))
	}
	created, ok := evs[0].(crmlead.CreatedEvent)
	if !ok {
		t.Fatalf("event type: %T", evs[0])
	}
	if created.LeadID != "lead-1" || created.TenantID != "tenant-1" || created.CreatedByMembershipID != "mem-creator" {
		t.Fatalf("event fields: %+v", created)
	}
	if created.SourcePurchaseID != "" {
		t.Fatalf("SourcePurchaseID should be empty on manual New, got %q", created.SourcePurchaseID)
	}
	// PullEvents drains.
	if got := l.PullEvents(); len(got) != 0 {
		t.Fatalf("second PullEvents should be empty, got %d", len(got))
	}
}

func TestNew_InvariantViolations(t *testing.T) {
	t.Parallel()
	withFixedClock(t)
	tests := []struct {
		name string
		mut  func(*crmlead.Profile)
		id   crmlead.ID
		tid  string
	}{
		{name: "missing id", mut: func(*crmlead.Profile) {}, id: "", tid: "t"},
		{name: "missing tenant", mut: func(*crmlead.Profile) {}, id: "id", tid: ""},
		{name: "missing contact_name", mut: func(p *crmlead.Profile) { p.ContactName = "" }, id: "id", tid: "t"},
		{name: "bad phone format", mut: func(p *crmlead.Profile) { p.PhoneE164 = "9876543210" }, id: "id", tid: "t"},
		{name: "bad phone length", mut: func(p *crmlead.Profile) { p.PhoneE164 = "+91987" }, id: "id", tid: "t"},
		{name: "bad pincode length", mut: func(p *crmlead.Profile) { p.Pincode = "12345" }, id: "id", tid: "t"},
		{name: "bad business_type", mut: func(p *crmlead.Profile) { p.BusinessType = "Wholesale" }, id: "id", tid: "t"},
		{name: "bad medicine_system", mut: func(p *crmlead.Profile) { p.MedicineSystem = "Homeopathic" }, id: "id", tid: "t"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validProfile()
			tc.mut(&p)
			_, err := crmlead.New(tc.id, tc.tid, p, "actor")
			if err == nil {
				t.Fatalf("want error")
			}
			if !errors.Is(err, crmlead.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNewFromPurchaseSnapshot_HappyPath(t *testing.T) {
	t.Parallel()
	withFixedClock(t)
	snap := crmlead.PurchaseSnapshot{
		PurchaseID:              "purchase-1",
		PlatformLeadID:          "pl-1",
		PurchasedByMembershipID: "buyer-1",
		ContactName:             "Naresh Pharma",
		MobileE164:              "+919812345678",
		Email:                   "naresh@example.com",
		PinCode:                 "560001",
		City:                    "Bengaluru",
		District:                "Bengaluru Urban",
		State:                   "Karnataka",
		Street:                  "MG Road 1",
		HasDrugLicence:          true,
		HasGst:                  true,
		GstNumber:               "29ABCDE1234F1Z5",
		HasPan:                  true,
		PanNumber:               "ABCDE1234F",
		BusinessType:            "PCD",
		MedicineSystem:          "Ayurvedic",
		ProductRanges:           []string{"Wellness"},
		DosageForms:             []string{"Syrup"},
		OrderValue:              "Above50000",
		BuyTimeline:             "Within15Days",
	}
	l, err := crmlead.NewFromPurchaseSnapshot("lead-2", "tenant-2", snap)
	if err != nil {
		t.Fatalf("NewFromPurchaseSnapshot: %v", err)
	}
	if l.SourcePurchaseID() != "purchase-1" {
		t.Fatalf("SourcePurchaseID: %q", l.SourcePurchaseID())
	}
	if l.SourcePlatformLeadID() != "pl-1" {
		t.Fatalf("SourcePlatformLeadID: %q", l.SourcePlatformLeadID())
	}
	if l.Profile().Extra.Email != "naresh@example.com" {
		t.Fatalf("Extra.Email: %q", l.Profile().Extra.Email)
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	if got := evs[0].(crmlead.CreatedEvent).SourcePurchaseID; got != "purchase-1" {
		t.Fatalf("CreatedEvent.SourcePurchaseID: %q", got)
	}
}

func TestNewFromPurchaseSnapshot_RejectsMissingPurchaseID(t *testing.T) {
	t.Parallel()
	withFixedClock(t)
	snap := crmlead.PurchaseSnapshot{
		ContactName: "X", MobileE164: "+919812345678",
	}
	_, err := crmlead.NewFromPurchaseSnapshot("lead-x", "t", snap)
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

// ----- Stage state-machine tests --------------------------------------------

func newLead(t *testing.T) *crmlead.CrmLead {
	t.Helper()
	withFixedClock(t)
	l, err := crmlead.New("lead-state", "tenant", validProfile(), "mem-actor")
	if err != nil {
		t.Fatalf("seed New: %v", err)
	}
	_ = l.PullEvents() // discard CreatedEvent so test asserts only the transition emit
	return l
}

func TestChangeStage_HappyForwardChain(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	for _, target := range []crmlead.Stage{crmlead.StageContacted, crmlead.StageInterested, crmlead.StageNegotiation} {
		if err := l.ChangeStage(target, "mem-sales", "advancing"); err != nil {
			t.Fatalf("ChangeStage %s: %v", target, err)
		}
		if l.Stage() != target {
			t.Fatalf("stage = %q, want %q", l.Stage(), target)
		}
		evs := l.PullEvents()
		if len(evs) != 1 {
			t.Fatalf("events on %s: %d", target, len(evs))
		}
		got, ok := evs[0].(crmlead.StageChangedEvent)
		if !ok || got.NewStage != target {
			t.Fatalf("event: %+v", evs[0])
		}
	}
}

func TestChangeStage_IdempotentSelfTransition(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.ChangeStage(crmlead.StageNew, "mem-sales", ""); err != nil {
		t.Fatalf("self ChangeStage: %v", err)
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Fatalf("self transition should emit no event, got %d", len(evs))
	}
}

func TestChangeStage_RejectsSkip(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	err := l.ChangeStage(crmlead.StageNegotiation, "mem-sales", "")
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestChangeStage_RejectsBacktrack(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.ChangeStage(crmlead.StageContacted, "mem-sales", ""); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = l.PullEvents()
	err := l.ChangeStage(crmlead.StageNew, "mem-sales", "")
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestChangeStage_RejectsDirectConvertOrLose(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	for _, target := range []crmlead.Stage{crmlead.StageConverted, crmlead.StageLost} {
		if err := l.ChangeStage(target, "mem", ""); !errors.Is(err, crmlead.ErrInvalid) {
			t.Fatalf("target %s: want ErrInvalid, got %v", target, err)
		}
	}
}

func TestChangeStage_TerminalRefused(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert("mem-closer"); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	_ = l.PullEvents()
	err := l.ChangeStage(crmlead.StageContacted, "mem", "")
	if !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
}

// ----- Temperature tests ----------------------------------------------------

func TestChangeTemperature_FreeTransition(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	for _, target := range []crmlead.Temperature{crmlead.TemperatureHot, crmlead.TemperatureCold, crmlead.TemperatureDead, crmlead.TemperatureWarm} {
		if err := l.ChangeTemperature(target, "mem"); err != nil {
			t.Fatalf("ChangeTemperature %s: %v", target, err)
		}
		if l.Temperature() != target {
			t.Fatalf("temperature: %q want %q", l.Temperature(), target)
		}
	}
}

func TestChangeTemperature_IdempotentSelf(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.ChangeTemperature(crmlead.TemperatureWarm, "mem"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Fatalf("self change should emit no event, got %d", len(evs))
	}
}

func TestChangeTemperature_RejectsTerminal(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Lose("mem", "no budget"); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	_ = l.PullEvents()
	err := l.ChangeTemperature(crmlead.TemperatureHot, "mem")
	if !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
}

// ----- Convert / Lose -------------------------------------------------------

func TestConvert_Terminal(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert("mem-closer"); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if l.Stage() != crmlead.StageConverted {
		t.Fatalf("stage: %q", l.Stage())
	}
	if !l.ConvertedAt().Equal(fixedNow) {
		t.Fatalf("ConvertedAt: %v", l.ConvertedAt())
	}
	if l.ConvertedByMembershipID() != "mem-closer" {
		t.Fatalf("ConvertedByMembershipID: %q", l.ConvertedByMembershipID())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	got, ok := evs[0].(crmlead.ConvertedEvent)
	if !ok || got.ConvertedByMembershipID != "mem-closer" {
		t.Fatalf("event: %+v", evs[0])
	}
	// Second Convert is refused.
	if err := l.Convert("mem-closer"); !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("second Convert: want ErrTerminal, got %v", err)
	}
}

func TestLose_TerminalWithReason(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Lose("mem-closer", "competitor won pricing"); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if l.Stage() != crmlead.StageLost {
		t.Fatalf("stage: %q", l.Stage())
	}
	if l.LostReason() != "competitor won pricing" {
		t.Fatalf("LostReason: %q", l.LostReason())
	}
	if got := l.PullEvents(); len(got) != 1 {
		t.Fatalf("events: %d", len(got))
	}
}

func TestLose_RequiresReason(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Lose("mem", ""); !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestLose_RejectsAfterConvert(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert("mem"); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	_ = l.PullEvents()
	err := l.Lose("mem", "too late")
	if !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
}

// ----- Assign tests ---------------------------------------------------------

func TestAssign_FirstAssignment(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Assign("mem-sales-A", "mem-manager", "initial routing"); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if l.AssigneeMembershipID() != "mem-sales-A" {
		t.Fatalf("AssigneeMembershipID: %q", l.AssigneeMembershipID())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	got, ok := evs[0].(crmlead.AssignedEvent)
	if !ok || got.PreviousAssignee != "" || got.AssigneeMembershipID != "mem-sales-A" {
		t.Fatalf("event: %+v", evs[0])
	}
}

func TestAssign_Reassignment(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Assign("mem-sales-A", "mem-manager", "initial"); err != nil {
		t.Fatalf("Assign 1: %v", err)
	}
	_ = l.PullEvents()
	if err := l.Assign("mem-sales-B", "mem-manager", "rebalance"); err != nil {
		t.Fatalf("Assign 2: %v", err)
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	got := evs[0].(crmlead.AssignedEvent)
	if got.PreviousAssignee != "mem-sales-A" || got.AssigneeMembershipID != "mem-sales-B" {
		t.Fatalf("event: %+v", got)
	}
}

func TestAssign_IdempotentSame(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Assign("mem-A", "mem-manager", ""); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	_ = l.PullEvents()
	if err := l.Assign("mem-A", "mem-manager", "ignored"); err != nil {
		t.Fatalf("Assign idempotent: %v", err)
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Fatalf("idempotent should emit no event, got %d", len(evs))
	}
}

func TestAssign_RejectsTerminal(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert("mem"); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	_ = l.PullEvents()
	err := l.Assign("mem-new", "mem-manager", "tried")
	if !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
}

// ----- Stage helpers --------------------------------------------------------

func TestStage_Catalog(t *testing.T) {
	t.Parallel()
	for _, s := range []crmlead.Stage{crmlead.StageNew, crmlead.StageContacted, crmlead.StageInterested, crmlead.StageNegotiation, crmlead.StageConverted, crmlead.StageLost} {
		if !s.IsValid() {
			t.Fatalf("Stage %q should be valid", s)
		}
	}
	if crmlead.Stage("nope").IsValid() {
		t.Fatal("unknown Stage should be invalid")
	}
	if !crmlead.StageConverted.IsTerminal() || !crmlead.StageLost.IsTerminal() {
		t.Fatal("converted/lost must be terminal")
	}
	if crmlead.StageNew.IsTerminal() {
		t.Fatal("new must NOT be terminal")
	}
}

func TestParseStage_HappyAndError(t *testing.T) {
	t.Parallel()
	if s, err := crmlead.ParseStage("contacted"); err != nil || s != crmlead.StageContacted {
		t.Fatalf("ParseStage(contacted): %v %v", s, err)
	}
	if _, err := crmlead.ParseStage("Maybe"); !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("ParseStage(Maybe): want ErrInvalid got %v", err)
	}
}

func TestParseTemperature_HappyAndError(t *testing.T) {
	t.Parallel()
	if temp, err := crmlead.ParseTemperature("cold"); err != nil || temp != crmlead.TemperatureCold {
		t.Fatalf("ParseTemperature: %v %v", temp, err)
	}
	if _, err := crmlead.ParseTemperature("lukewarm"); !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("ParseTemperature: want ErrInvalid got %v", err)
	}
}

// ----- UnmarshalFromDB ------------------------------------------------------

func TestUnmarshalFromDB_RoundTrip(t *testing.T) {
	t.Parallel()
	snap := crmlead.Snapshot{
		ID:                      "lead-r",
		TenantID:                "tenant-r",
		Profile:                 validProfile(),
		Stage:                   crmlead.StageNegotiation,
		Temperature:             crmlead.TemperatureHot,
		SourcePurchaseID:        "p-1",
		SourcePlatformLeadID:    "pl-1",
		AssigneeMembershipID:    "mem-A",
		AssignedAt:              fixedNow,
		CreatedAt:               fixedNow,
		CreatedByMembershipID:   "mem-buyer",
	}
	l := crmlead.UnmarshalFromDB(snap)
	if l.Stage() != crmlead.StageNegotiation || l.Temperature() != crmlead.TemperatureHot {
		t.Fatalf("stage/temp: %s %s", l.Stage(), l.Temperature())
	}
	if l.SourcePurchaseID() != "p-1" || l.SourcePlatformLeadID() != "pl-1" {
		t.Fatalf("source: %q %q", l.SourcePurchaseID(), l.SourcePlatformLeadID())
	}
	if l.AssigneeMembershipID() != "mem-A" {
		t.Fatalf("assignee: %q", l.AssigneeMembershipID())
	}
	if l.PullEvents() != nil {
		t.Fatal("UnmarshalFromDB should emit no events")
	}
}
