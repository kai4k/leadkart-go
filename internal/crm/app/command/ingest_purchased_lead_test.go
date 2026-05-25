package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ingestTenantID is the tenant used by the ingest tests. Distinct from
// [testTenantID] (seedLead's) because ingest tests don't seed leads
// upfront — they ingest into a fresh tenant scope.
const ingestTenantID = tenant.ID("01923400-0000-7000-8000-bbbbbbbb0001")

func validSnapshot(purchase string) crmlead.PurchaseSnapshot {
	return crmlead.PurchaseSnapshot{
		PurchaseID:              purchase,
		PlatformLeadID:          "01923400-aaaa-7000-8000-000000000001",
		PurchasedByMembershipID: "01923400-bbbb-7000-8000-000000000002",
		ContactName:             "Test Pharma",
		MobileE164:              "+919812345678",
		Email:                   "x@example.com",
		PinCode:                 "411001",
		City:                    "Pune",
		District:                "Pune",
		State:                   "Maharashtra",
		HasDrugLicence:          true,
		HasGst:                  true,
		BusinessType:            "PCD",
		MedicineSystem:          "Allopathic",
		OrderValue:              "Upto25000",
		BuyTimeline:             "WithinWeek",
		ProductRanges:           []string{"Antibiotics"},
		DosageForms:             []string{"Tablet"},
	}
}

func TestIngest_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := command.NewIngestPurchasedLeadHandler(leads, fixedTime, newTestLeadID)
	out, err := h.Handle(t.Context(), command.IngestPurchasedLeadCommand{
		PurchaseID:              "01923400-0000-7000-8000-dddddddd0001",
		TenantID:                ingestTenantID,
		PlatformLeadID:          "01923400-0000-7000-8000-eeeeeeee0001",
		PurchasedByMembershipID: "01923400-0000-7000-8000-cccccccc000d",
		Snapshot:                validSnapshot("01923400-0000-7000-8000-dddddddd0001"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.LeadID.IsZero() {
		t.Fatal("LeadID should be set")
	}
	if out.AlreadyExisted {
		t.Fatal("AlreadyExisted should be false on first ingest")
	}
}

func TestIngest_IdempotentOnSamePurchaseID(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := command.NewIngestPurchasedLeadHandler(leads, fixedTime, newTestLeadID)
	cmd := command.IngestPurchasedLeadCommand{
		PurchaseID: "01923400-0000-7000-8000-dddddddd0002", TenantID: ingestTenantID,
		Snapshot: validSnapshot("01923400-0000-7000-8000-dddddddd0002"),
	}
	first, err := h.Handle(t.Context(), cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := h.Handle(t.Context(), cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.AlreadyExisted {
		t.Fatal("second call should report AlreadyExisted")
	}
	if second.LeadID != first.LeadID {
		t.Fatalf("LeadID mismatch: first=%s second=%s", first.LeadID, second.LeadID)
	}
}

func TestIngest_MissingPurchaseID(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := command.NewIngestPurchasedLeadHandler(leads, fixedTime, newTestLeadID)
	_, err := h.Handle(t.Context(), command.IngestPurchasedLeadCommand{TenantID: "t", Snapshot: validSnapshot("")})
	if err == nil {
		t.Fatal("want error")
	}
}

func TestIngest_NormalisesProvenanceOnSnapshot(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := command.NewIngestPurchasedLeadHandler(leads, fixedTime, newTestLeadID)
	// Snapshot fields left blank — handler should fill them from the
	// command top-level so the persisted aggregate carries source info.
	snap := validSnapshot("01923400-0000-7000-8000-dddddddd0003")
	snap.PurchaseID = ""
	snap.PlatformLeadID = ""
	snap.PurchasedByMembershipID = ""
	out, err := h.Handle(t.Context(), command.IngestPurchasedLeadCommand{
		PurchaseID:              "01923400-0000-7000-8000-dddddddd0003",
		TenantID:                ingestTenantID,
		PlatformLeadID:          "01923400-0000-7000-8000-eeeeeeee0003",
		PurchasedByMembershipID: "01923400-0000-7000-8000-cccccccc000e",
		Snapshot:                snap,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	l, err := leads.GetByID(t.Context(), ingestTenantID, out.LeadID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if l.SourcePurchaseID() != "01923400-0000-7000-8000-dddddddd0003" || l.SourcePlatformLeadID() != "01923400-0000-7000-8000-eeeeeeee0003" {
		t.Fatalf("provenance not normalised: purchase=%q platform=%q", l.SourcePurchaseID(), l.SourcePlatformLeadID())
	}
}

func TestIngest_LookupFailureBubbles(t *testing.T) {
	t.Parallel()
	leads := &erroringLeads{FakeRepository: newFakeLeads()}
	h := command.NewIngestPurchasedLeadHandler(leads, fixedTime, newTestLeadID)
	_, err := h.Handle(t.Context(), command.IngestPurchasedLeadCommand{
		PurchaseID: "p", TenantID: "t", Snapshot: validSnapshot("p"),
	})
	if err == nil || errors.Is(err, command.ErrPurchaseAlreadyIngested) {
		t.Fatalf("want generic error, got %v", err)
	}
}

// erroringLeads always errors on GetBySourcePurchaseID (non-NotFound),
// exercising the bubble-up path. Embeds the canonical
// *crmleadtest.FakeRepository so the embedded contract methods carry
// through unchanged; only GetBySourcePurchaseID is overridden.
type erroringLeads struct {
	*crmleadtest.FakeRepository
}

func (*erroringLeads) GetBySourcePurchaseID(_ context.Context, _ tenant.ID, _ string) (*crmlead.CrmLead, error) {
	return nil, errors.New("synthetic lookup failure")
}
