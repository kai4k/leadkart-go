package crmlead_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedNow is the deterministic clock used across the table tests so
// the emitted-event timestamps are predictable.
var fixedNow = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

// Test fixture UUIDs. All aggregate IDs / tenant IDs / membership IDs
// MUST parse as RFC 9562 UUIDs (per reviewer H6 — aggregate
// construction validates UUIDness). The deterministic suffix below
// makes the test events trivially diffable.
const (
	tidLead1         = "01923400-0000-7000-8000-aaaaaaaa0001"
	tidLead2         = "01923400-0000-7000-8000-aaaaaaaa0002"
	tidLeadX         = "01923400-0000-7000-8000-aaaaaaaa0009"
	tidLeadState     = "01923400-0000-7000-8000-aaaaaaaa0010"
	tidTenant1       = "01923400-0000-7000-8000-bbbbbbbb0001"
	tidTenant2       = "01923400-0000-7000-8000-bbbbbbbb0002"
	tidMemCreator    = "01923400-0000-7000-8000-cccccccc0001"
	tidMemActor      = "01923400-0000-7000-8000-cccccccc0002"
	tidMemSales      = "01923400-0000-7000-8000-cccccccc0003"
	tidMemSalesA     = "01923400-0000-7000-8000-cccccccc0004"
	tidMemSalesB     = "01923400-0000-7000-8000-cccccccc0005"
	tidMemManager    = "01923400-0000-7000-8000-cccccccc0006"
	tidMemCloser     = "01923400-0000-7000-8000-cccccccc0007"
	tidMemNew        = "01923400-0000-7000-8000-cccccccc0008"
	tidMemA          = "01923400-0000-7000-8000-cccccccc0009"
	tidMem           = "01923400-0000-7000-8000-cccccccc000a"
	tidActor         = "01923400-0000-7000-8000-cccccccc000b"
	tidPurchase1     = "01923400-0000-7000-8000-dddddddd0001"
	tidPlatformLead1 = "01923400-0000-7000-8000-eeeeeeee0001"
	tidBuyer1        = "01923400-0000-7000-8000-cccccccc000c"
)

// Typed tenant-ID fixtures. The crmlead factories take tenant.ID
// (typed alias) per TestArch_NoBareTenantIDStrings — tests use the
// typed values directly so the test stays parity with production.
var (
	tenantID1 = tenant.ID(tidTenant1)
	tenantID2 = tenant.ID(tidTenant2)
)

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

	l, err := crmlead.New(crmlead.ID(tidLead1), tenantID1, validProfile(), tidMemCreator, fixedNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if l.ID() != crmlead.ID(tidLead1) {
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
	if created.LeadID != crmlead.ID(tidLead1) || created.TenantID != tenantID1 || created.CreatedByMembershipID != tidMemCreator {
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
	tests := []struct {
		name string
		mut  func(*crmlead.Profile)
		id   crmlead.ID
		tid  tenant.ID
	}{
		{name: "missing id", mut: func(*crmlead.Profile) {}, id: "", tid: tenantID1},
		{name: "non-uuid id", mut: func(*crmlead.Profile) {}, id: crmlead.ID("not-a-uuid"), tid: tenantID1},
		{name: "missing tenant", mut: func(*crmlead.Profile) {}, id: crmlead.ID(tidLead1), tid: ""},
		{name: "non-uuid tenant", mut: func(*crmlead.Profile) {}, id: crmlead.ID(tidLead1), tid: tenant.ID("not-a-uuid")},
		{name: "missing contact_name", mut: func(p *crmlead.Profile) { p.ContactName = "" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad phone format", mut: func(p *crmlead.Profile) { p.PhoneE164 = "9876543210" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad phone length", mut: func(p *crmlead.Profile) { p.PhoneE164 = "+91987" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad pincode too short", mut: func(p *crmlead.Profile) { p.Pincode = "12345" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad pincode too long", mut: func(p *crmlead.Profile) { p.Pincode = "1234567" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad pincode leading zero", mut: func(p *crmlead.Profile) { p.Pincode = "000000" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad business_type", mut: func(p *crmlead.Profile) { p.BusinessType = "Wholesale" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad medicine_system", mut: func(p *crmlead.Profile) { p.MedicineSystem = "Homeopathic" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad order_value", mut: func(p *crmlead.Profile) { p.OrderValue = "Below100" }, id: crmlead.ID(tidLead1), tid: tenantID1},
		{name: "bad buy_timeline", mut: func(p *crmlead.Profile) { p.BuyTimeline = "Tomorrow" }, id: crmlead.ID(tidLead1), tid: tenantID1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validProfile()
			tc.mut(&p)
			_, err := crmlead.New(tc.id, tc.tid, p, tidActor, fixedNow)
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
	snap := crmlead.PurchaseSnapshot{
		PurchaseID:              tidPurchase1,
		PlatformLeadID:          tidPlatformLead1,
		PurchasedByMembershipID: tidBuyer1,
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
	l, err := crmlead.NewFromPurchaseSnapshot(crmlead.ID(tidLead2), tenantID2, snap, fixedNow)
	if err != nil {
		t.Fatalf("NewFromPurchaseSnapshot: %v", err)
	}
	if l.SourcePurchaseID() != tidPurchase1 {
		t.Fatalf("SourcePurchaseID: %q", l.SourcePurchaseID())
	}
	if l.SourcePlatformLeadID() != tidPlatformLead1 {
		t.Fatalf("SourcePlatformLeadID: %q", l.SourcePlatformLeadID())
	}
	if l.Profile().Extra.Email != "naresh@example.com" {
		t.Fatalf("Extra.Email: %q", l.Profile().Extra.Email)
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	if got := evs[0].(crmlead.CreatedEvent).SourcePurchaseID; got != tidPurchase1 {
		t.Fatalf("CreatedEvent.SourcePurchaseID: %q", got)
	}
}

func TestNewFromPurchaseSnapshot_RejectsMissingPurchaseID(t *testing.T) {
	t.Parallel()
	snap := crmlead.PurchaseSnapshot{
		ContactName: "X", MobileE164: "+919812345678",
	}
	_, err := crmlead.NewFromPurchaseSnapshot(crmlead.ID(tidLeadX), tenantID1, snap, fixedNow)
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

// ----- Stage state-machine tests --------------------------------------------

func newLead(t *testing.T) *crmlead.CrmLead {
	t.Helper()
	l, err := crmlead.New(crmlead.ID(tidLeadState), tenantID1, validProfile(), tidMemActor, fixedNow)
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
		if err := l.ChangeStage(target, tidMemSales, "advancing", fixedNow); err != nil {
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
	if err := l.ChangeStage(crmlead.StageNew, tidMemSales, "", fixedNow); err != nil {
		t.Fatalf("self ChangeStage: %v", err)
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Fatalf("self transition should emit no event, got %d", len(evs))
	}
}

func TestChangeStage_RejectsSkip(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	err := l.ChangeStage(crmlead.StageNegotiation, tidMemSales, "", fixedNow)
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestChangeStage_RejectsBacktrack(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.ChangeStage(crmlead.StageContacted, tidMemSales, "", fixedNow); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = l.PullEvents()
	err := l.ChangeStage(crmlead.StageNew, tidMemSales, "", fixedNow)
	if !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestChangeStage_RejectsDirectConvertOrLose(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	for _, target := range []crmlead.Stage{crmlead.StageConverted, crmlead.StageLost} {
		if err := l.ChangeStage(target, tidMem, "", fixedNow); !errors.Is(err, crmlead.ErrInvalid) {
			t.Fatalf("target %s: want ErrInvalid, got %v", target, err)
		}
	}
}

func TestChangeStage_TerminalRefused(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert(tidMemCloser, fixedNow); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	_ = l.PullEvents()
	err := l.ChangeStage(crmlead.StageContacted, tidMem, "", fixedNow)
	if !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
}

// ----- Temperature tests ----------------------------------------------------

func TestChangeTemperature_FreeTransition(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	for _, target := range []crmlead.Temperature{crmlead.TemperatureHot, crmlead.TemperatureCold, crmlead.TemperatureDead, crmlead.TemperatureWarm} {
		if err := l.ChangeTemperature(target, tidMem, fixedNow); err != nil {
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
	if err := l.ChangeTemperature(crmlead.TemperatureWarm, tidMem, fixedNow); err != nil {
		t.Fatalf("err: %v", err)
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Fatalf("self change should emit no event, got %d", len(evs))
	}
}

func TestChangeTemperature_RejectsTerminal(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Lose(tidMem, "no budget", fixedNow); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	_ = l.PullEvents()
	err := l.ChangeTemperature(crmlead.TemperatureHot, tidMem, fixedNow)
	if !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
}

// ----- Convert / Lose -------------------------------------------------------

func TestConvert_Terminal(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert(tidMemCloser, fixedNow); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if l.Stage() != crmlead.StageConverted {
		t.Fatalf("stage: %q", l.Stage())
	}
	if !l.ConvertedAt().Equal(fixedNow) {
		t.Fatalf("ConvertedAt: %v", l.ConvertedAt())
	}
	if l.ConvertedByMembershipID() != tidMemCloser {
		t.Fatalf("ConvertedByMembershipID: %q", l.ConvertedByMembershipID())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	got, ok := evs[0].(crmlead.ConvertedEvent)
	if !ok || got.ConvertedByMembershipID != tidMemCloser {
		t.Fatalf("event: %+v", evs[0])
	}
	// Second Convert is refused.
	if err := l.Convert(tidMemCloser, fixedNow); !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("second Convert: want ErrTerminal, got %v", err)
	}
}

func TestLose_TerminalWithReason(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Lose(tidMemCloser, "competitor won pricing", fixedNow); err != nil {
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
	if err := l.Lose(tidMem, "", fixedNow); !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestLose_RejectsAfterConvert(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert(tidMem, fixedNow); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	_ = l.PullEvents()
	err := l.Lose(tidMem, "too late", fixedNow)
	if !errors.Is(err, crmlead.ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
}

// ----- Assign tests ---------------------------------------------------------

func TestAssign_FirstAssignment(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Assign(tidMemSalesA, tidMemManager, "initial routing", fixedNow); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if l.AssigneeMembershipID() != tidMemSalesA {
		t.Fatalf("AssigneeMembershipID: %q", l.AssigneeMembershipID())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	got, ok := evs[0].(crmlead.AssignedEvent)
	if !ok || got.PreviousAssignee != "" || got.AssigneeMembershipID != tidMemSalesA {
		t.Fatalf("event: %+v", evs[0])
	}
}

func TestAssign_Reassignment(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Assign(tidMemSalesA, tidMemManager, "initial", fixedNow); err != nil {
		t.Fatalf("Assign 1: %v", err)
	}
	_ = l.PullEvents()
	if err := l.Assign(tidMemSalesB, tidMemManager, "rebalance", fixedNow); err != nil {
		t.Fatalf("Assign 2: %v", err)
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	got := evs[0].(crmlead.AssignedEvent)
	if got.PreviousAssignee != tidMemSalesA || got.AssigneeMembershipID != tidMemSalesB {
		t.Fatalf("event: %+v", got)
	}
}

func TestAssign_IdempotentSame(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Assign(tidMemA, tidMemManager, "", fixedNow); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	_ = l.PullEvents()
	if err := l.Assign(tidMemA, tidMemManager, "ignored", fixedNow); err != nil {
		t.Fatalf("Assign idempotent: %v", err)
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Fatalf("idempotent should emit no event, got %d", len(evs))
	}
}

func TestAssign_RejectsTerminal(t *testing.T) {
	t.Parallel()
	l := newLead(t)
	if err := l.Convert(tidMem, fixedNow); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	_ = l.PullEvents()
	err := l.Assign(tidMemNew, tidMemManager, "tried", fixedNow)
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
		SourcePlatformLeadID:    tidPlatformLead1,
		AssigneeMembershipID:    tidMemA,
		AssignedAt:              fixedNow,
		CreatedAt:               fixedNow,
		CreatedByMembershipID:   "mem-buyer",
	}
	l := crmlead.UnmarshalFromDB(snap)
	if l.Stage() != crmlead.StageNegotiation || l.Temperature() != crmlead.TemperatureHot {
		t.Fatalf("stage/temp: %s %s", l.Stage(), l.Temperature())
	}
	if l.SourcePurchaseID() != "p-1" || l.SourcePlatformLeadID() != tidPlatformLead1 {
		t.Fatalf("source: %q %q", l.SourcePurchaseID(), l.SourcePlatformLeadID())
	}
	if l.AssigneeMembershipID() != tidMemA {
		t.Fatalf("assignee: %q", l.AssigneeMembershipID())
	}
	if l.PullEvents() != nil {
		t.Fatal("UnmarshalFromDB should emit no events")
	}
}
