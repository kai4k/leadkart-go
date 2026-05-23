package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

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
	h := command.NewIngestPurchasedLeadHandler(leads)
	out, err := h.Handle(context.Background(), command.IngestPurchasedLeadCommand{
		PurchaseID:              "purchase-1",
		TenantID:                "tenant-1",
		PlatformLeadID:          "pl-1",
		PurchasedByMembershipID: "mem-buyer",
		Snapshot:                validSnapshot("purchase-1"),
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
	h := command.NewIngestPurchasedLeadHandler(leads)
	cmd := command.IngestPurchasedLeadCommand{
		PurchaseID: "purchase-2", TenantID: "tenant-1",
		Snapshot: validSnapshot("purchase-2"),
	}
	first, err := h.Handle(context.Background(), cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := h.Handle(context.Background(), cmd)
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
	h := command.NewIngestPurchasedLeadHandler(leads)
	_, err := h.Handle(context.Background(), command.IngestPurchasedLeadCommand{TenantID: "t", Snapshot: validSnapshot("")})
	if err == nil {
		t.Fatal("want error")
	}
}

func TestIngest_NormalisesProvenanceOnSnapshot(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := command.NewIngestPurchasedLeadHandler(leads)
	// Snapshot fields left blank — handler should fill them from the
	// command top-level so the persisted aggregate carries source info.
	snap := validSnapshot("purchase-3")
	snap.PurchaseID = ""
	snap.PlatformLeadID = ""
	snap.PurchasedByMembershipID = ""
	out, err := h.Handle(context.Background(), command.IngestPurchasedLeadCommand{
		PurchaseID:              "purchase-3",
		TenantID:                "tenant-1",
		PlatformLeadID:          "pl-3",
		PurchasedByMembershipID: "mem-buyer-3",
		Snapshot:                snap,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	l, err := leads.GetByID(context.Background(), out.LeadID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if l.SourcePurchaseID() != "purchase-3" || l.SourcePlatformLeadID() != "pl-3" {
		t.Fatalf("provenance not normalised: purchase=%q platform=%q", l.SourcePurchaseID(), l.SourcePlatformLeadID())
	}
}

func TestIngest_LookupFailureBubbles(t *testing.T) {
	t.Parallel()
	leads := &erroringLeads{fakeLeads: newFakeLeads()}
	h := command.NewIngestPurchasedLeadHandler(leads)
	_, err := h.Handle(context.Background(), command.IngestPurchasedLeadCommand{
		PurchaseID: "p", TenantID: "t", Snapshot: validSnapshot("p"),
	})
	if err == nil || errors.Is(err, command.ErrPurchaseAlreadyIngested) {
		t.Fatalf("want generic error, got %v", err)
	}
}

// erroringLeads always errors on GetBySourcePurchaseID (non-NotFound),
// exercising the bubble-up path. Embeds *fakeLeads so the sync.Mutex
// inside fakeLeads is shared by pointer, not copied by value.
type erroringLeads struct {
	*fakeLeads
}

func (*erroringLeads) GetBySourcePurchaseID(_ context.Context, _ string) (*crmlead.CrmLead, error) {
	return nil, errors.New("synthetic lookup failure")
}
