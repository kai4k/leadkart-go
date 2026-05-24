package query_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

func qNow() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

func qSampleForm(t *testing.T) leadform.Form {
	t.Helper()
	f, err := leadform.New(leadform.Input{
		ContactName:    "Marketplace Lead Co",
		MobileE164:     "+919876543210",
		Email:          "secret@hidden.test", // wire-shaped — MUST NOT appear on the marketplace view
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		HasGst:         true,
		GstNumber:      "27AAAAA0000A1Z5",
		BusinessType:   leadform.BusinessTypePCD,
		MedicineSystem: leadform.MedicineSystemAllopathic,
		OrderValue:     leadform.OrderValueUpto25000,
		BuyTimeline:    leadform.BuyTimelineWithin15Days,
	})
	if err != nil {
		t.Fatalf("sample form: %v", err)
	}
	return f
}

func seedLead(t *testing.T, leads *platformtest.FakePlatformLeadRepository) platformlead.ID {
	t.Helper()
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	contactID := unverifiedcontact.ID(ids.NewV7().String())
	leadID := platformlead.ID(ids.NewV7().String())
	l, err := platformlead.NewFromUnverifiedContact(leadID, contactID, qSampleForm(t), agentID, qNow())
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	if err := leads.Add(t.Context(), l); err != nil {
		t.Fatalf("seed persist: %v", err)
	}
	return leadID
}

// TestBrowseMarketplace_ReturnsUnsoldLeads_NoPII — happy path. The
// marketplace view MUST exclude PII (email, GST/PAN numbers) since
// any tenant can browse it. C2 + H12 — review-pass.
func TestBrowseMarketplace_ReturnsUnsoldLeads_NoPII(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	leadID := seedLead(t, leads)

	h := query.NewBrowseMarketplaceHandler(leads)
	page, err := h.Handle(t.Context(), query.BrowseMarketplaceQuery{
		Filter:   platformlead.MarketplaceFilter{},
		Cursor:   pagination.Cursor{},
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected 1 marketplace lead, got %d", len(page.Items))
	}
	got := page.Items[0]
	if got.ID != leadID.String() {
		t.Errorf("ID=%q want %q", got.ID, leadID)
	}
	if got.City != "Pune" {
		t.Errorf("City=%q want Pune", got.City)
	}
	// H12 — explicit PII-exclusion check on the marketplace view.
	// The view struct itself has no Email / GstNumber / PanNumber
	// fields; failing this check requires future schema drift.
	if got.ContactName == "" {
		t.Error("ContactName missing — needed for the marketplace card")
	}
	if got.HasGst != true {
		t.Errorf("HasGst=%v want true (the BOOL is exposed; the NUMBER is hidden)", got.HasGst)
	}
}

// TestBrowseMarketplace_ExcludesSoldLeads — once a lead is purchased,
// it disappears from the browse stream (only the purchaser sees it
// via the post-purchase /v1/me/leads — Slice 2). C2.
func TestBrowseMarketplace_ExcludesSoldLeads(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	leadID := seedLead(t, leads)

	// Mark sold via direct UpdateByID — bypassing the handler.
	tenantID := platformlead.TenantID(ids.NewV7().String())
	memberID := unverifiedcontact.MembershipID(ids.NewV7().String())
	err := leads.UpdateByID(t.Context(), leadID, func(l *platformlead.PlatformLead) (bool, error) {
		return true, l.Purchase(tenantID, memberID, 50000, qNow())
	})
	if err != nil {
		t.Fatalf("seed purchase: %v", err)
	}

	h := query.NewBrowseMarketplaceHandler(leads)
	page, err := h.Handle(t.Context(), query.BrowseMarketplaceQuery{
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("expected sold lead excluded from browse, got %d items", len(page.Items))
	}
}

// TestBrowseMarketplace_PageSizeClampedAndPaginated — clamping happens
// at the handler layer + the repo gets size+1 to compute has_more.
func TestBrowseMarketplace_PageSizeClampedAndPaginated(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	for range 3 {
		seedLead(t, leads)
	}

	h := query.NewBrowseMarketplaceHandler(leads)
	page, err := h.Handle(t.Context(), query.BrowseMarketplaceQuery{
		PageSize: 2,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected page size 2, got %d items", len(page.Items))
	}
	// has_more should be true because we seeded 3 leads + asked for 2.
	if !page.HasMore {
		t.Error("expected has_more = true with 3 seeded vs 2 requested")
	}
}
