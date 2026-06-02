package platformlead_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

var (
	leadID     = platformlead.ID("01900000-0000-7000-8000-000000000020")
	contactID  = unverifiedcontact.ID("01900000-0000-7000-8000-000000000010")
	tenantA    = platformlead.TenantID("01900000-0000-7000-8000-000000000200")
	tenantB    = platformlead.TenantID("01900000-0000-7000-8000-000000000201")
	agentID    = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000001")
	memberA    = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000301")
	memberB    = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000302")
	purchaseP1 = "01900000-0000-7000-8000-000000000400"
	purchaseP2 = "01900000-0000-7000-8000-000000000401"
	now        = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
)

// tierLimit is the default sale limit fed to RecordPurchase in unit tests
// (the real value comes from platform.lead_tiers).
const tierLimit = 6

func sampleForm(t *testing.T) leadform.Form {
	t.Helper()
	f, err := leadform.New(leadform.Input{
		ContactName:    "Acme Pharma",
		MobileE164:     "+919876543210",
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		HasGst:         false,
		HasPan:         false,
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

func TestNewFromUnverifiedContact_HappyPath(t *testing.T) {
	t.Parallel()
	l, err := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if l.Tier() != platformlead.TierStandard {
		t.Errorf("default tier = %q, want standard", l.Tier())
	}
	if !l.IsAvailable(tierLimit) {
		t.Error("expected fresh lead to be available")
	}
	if l.PurchaseCount() != 0 {
		t.Errorf("fresh lead PurchaseCount = %d, want 0", l.PurchaseCount())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if _, ok := evs[0].(platformlead.VerifiedEvent); !ok {
		t.Errorf("expected VerifiedEvent, got %T", evs[0])
	}
}

func TestNewFromUnverifiedContact_RejectsZeroFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		id        platformlead.ID
		contactID unverifiedcontact.ID
		agentID   unverifiedcontact.MembershipID
	}{
		{"empty id", "", contactID, agentID},
		{"empty contact id", leadID, "", agentID},
		{"empty agent id", leadID, contactID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := platformlead.NewFromUnverifiedContact(tc.id, tc.contactID, sampleForm(t), tc.agentID, now)
			if !errors.Is(err, platformlead.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNewFromUnverifiedContact_RejectsZeroNow(t *testing.T) {
	t.Parallel()
	_, err := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, time.Time{})
	if !errors.Is(err, platformlead.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNewFromUnverifiedContact_RejectsEmptyContactName(t *testing.T) {
	t.Parallel()
	// Build an invalid-form fixture via UnmarshalFromDB to bypass
	// leadform.New's own ctor validation; the platformlead ctor must reject it.
	emptyForm := leadform.UnmarshalFromDB(leadform.Input{
		ContactName: "   ", // whitespace-only fails the TrimSpace check
		MobileE164:  "+919876543210",
	})
	_, err := platformlead.NewFromUnverifiedContact(leadID, contactID, emptyForm, agentID, now)
	if !errors.Is(err, platformlead.ErrInvalid) {
		t.Errorf("expected ErrInvalid for empty ContactName, got %v", err)
	}
}

func TestRecordPurchase_HappyPath(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	_ = l.PullEvents()
	at := now.Add(time.Hour)
	if err := l.RecordPurchase(purchaseP1, tenantA, memberA, 50000, tierLimit, at); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if l.PurchaseCount() != 1 {
		t.Errorf("PurchaseCount = %d, want 1", l.PurchaseCount())
	}
	if !l.HasBuyer(tenantA) {
		t.Error("expected tenantA to be a buyer")
	}
	pending := l.PullPendingPurchases()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending purchase, got %d", len(pending))
	}
	if pending[0].TenantID() != tenantA || pending[0].AmountPaisa() != 50000 || pending[0].ID() != purchaseP1 {
		t.Errorf("pending purchase = %+v", pending[0])
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	pe, ok := evs[0].(platformlead.PurchasedEvent)
	if !ok {
		t.Fatalf("expected PurchasedEvent, got %T", evs[0])
	}
	if pe.TenantID != tenantA || pe.AmountPaisa != 50000 || pe.PurchaseID != purchaseP1 {
		t.Errorf("event = %+v", pe)
	}
}

func TestRecordPurchase_MultipleBuyers_UntilLimit(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	// Limit of 2: two distinct tenants succeed.
	if err := l.RecordPurchase(purchaseP1, tenantA, memberA, 50000, 2, now.Add(time.Hour)); err != nil {
		t.Fatalf("buyer A: %v", err)
	}
	if err := l.RecordPurchase(purchaseP2, tenantB, memberB, 50000, 2, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("buyer B: %v", err)
	}
	if l.PurchaseCount() != 2 {
		t.Fatalf("PurchaseCount = %d, want 2", l.PurchaseCount())
	}
	if l.IsAvailable(2) {
		t.Error("lead at limit must not be available")
	}
}

func TestRecordPurchase_SoldOut(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	// Limit of 1: first buyer succeeds, second is sold out.
	if err := l.RecordPurchase(purchaseP1, tenantA, memberA, 50000, 1, now.Add(time.Hour)); err != nil {
		t.Fatalf("buyer A: %v", err)
	}
	err := l.RecordPurchase(purchaseP2, tenantB, memberB, 50000, 1, now.Add(2*time.Hour))
	if !errors.Is(err, platformlead.ErrSoldOut) {
		t.Errorf("expected ErrSoldOut, got %v", err)
	}
}

func TestRecordPurchase_PerLeadSaleLimitOverride(t *testing.T) {
	t.Parallel()
	override := 1
	l := platformlead.UnmarshalFromDB(platformlead.Snapshot{
		ID: leadID, SourceContactID: contactID, Form: sampleForm(t),
		Tier: platformlead.TierStandard, SaleLimit: &override,
		VerifiedAt: now, VerifiedByMembershipID: agentID, CreatedAt: now,
	})
	// Per-lead override (1) wins over the tier default (6).
	if err := l.RecordPurchase(purchaseP1, tenantA, memberA, 50000, 6, now.Add(time.Hour)); err != nil {
		t.Fatalf("buyer A: %v", err)
	}
	err := l.RecordPurchase(purchaseP2, tenantB, memberB, 50000, 6, now.Add(2*time.Hour))
	if !errors.Is(err, platformlead.ErrSoldOut) {
		t.Errorf("expected ErrSoldOut under per-lead override, got %v", err)
	}
}

func TestRecordPurchase_DoubleBuySameTenant_Rejected(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	_ = l.RecordPurchase(purchaseP1, tenantA, memberA, 50000, tierLimit, now.Add(time.Hour)) // arch-test:ignore-err — domain test seed
	err := l.RecordPurchase(purchaseP2, tenantA, memberA, 75000, tierLimit, now.Add(2*time.Hour))
	if !errors.Is(err, platformlead.ErrAlreadyPurchased) {
		t.Errorf("expected ErrAlreadyPurchased on same-tenant re-buy, got %v", err)
	}
}

func TestRecordPurchase_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		purchaseID   string
		tenant       platformlead.TenantID
		membership   unverifiedcontact.MembershipID
		amount       int64
		defaultLimit int
	}{
		{"empty purchase id", "", tenantA, memberA, 50000, tierLimit},
		{"non-uuid purchase id", "nope", tenantA, memberA, 50000, tierLimit},
		{"empty tenant", purchaseP1, "", memberA, 50000, tierLimit},
		{"empty membership", purchaseP1, tenantA, "", 50000, tierLimit},
		{"zero amount", purchaseP1, tenantA, memberA, 0, tierLimit},
		{"negative amount", purchaseP1, tenantA, memberA, -1, tierLimit},
		{"zero default limit", purchaseP1, tenantA, memberA, 50000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
			err := l.RecordPurchase(tc.purchaseID, tc.tenant, tc.membership, tc.amount, tc.defaultLimit, now.Add(time.Hour))
			if !errors.Is(err, platformlead.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestUnmarshalFromDB_RoundTrip(t *testing.T) {
	t.Parallel()
	snap := platformlead.Snapshot{
		ID:                     leadID,
		SourceContactID:        contactID,
		Form:                   sampleForm(t),
		Tier:                   platformlead.TierPriority,
		BuyerTenantIDs:         []platformlead.TenantID{tenantA},
		VerifiedAt:             now,
		VerifiedByMembershipID: agentID,
		CreatedAt:              now,
	}
	l := platformlead.UnmarshalFromDB(snap)
	if l.Tier() != platformlead.TierPriority {
		t.Errorf("Tier round-trip = %q", l.Tier())
	}
	if l.PurchaseCount() != 1 || !l.HasBuyer(tenantA) {
		t.Errorf("buyer set round-trip failed: count=%d", l.PurchaseCount())
	}
}

// ----- Getter coverage ------------------------------------------------------

func TestGetters_PinAllConstructionTimeValues(t *testing.T) {
	t.Parallel()
	form := sampleForm(t)
	l, err := platformlead.NewFromUnverifiedContact(leadID, contactID, form, agentID, now)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if l.ID() != leadID {
		t.Errorf("ID = %q", l.ID())
	}
	if l.SourceContactID() != contactID {
		t.Errorf("SourceContactID = %q", l.SourceContactID())
	}
	if l.Form().ContactName() != form.ContactName() {
		t.Errorf("Form() round-trip failed")
	}
	if l.GstVerified() {
		t.Error("GstVerified default true, want false (Phase 2 feature)")
	}
	if l.VerifiedByMembershipID() != agentID {
		t.Errorf("VerifiedByMembershipID = %q", l.VerifiedByMembershipID())
	}
	if !l.VerifiedAt().Equal(now) {
		t.Errorf("VerifiedAt = %v", l.VerifiedAt())
	}
	if !l.CreatedAt().Equal(now) {
		t.Errorf("CreatedAt = %v", l.CreatedAt())
	}
	// Fresh lead: standard tier, no per-lead override, no buyers.
	if l.Tier() != platformlead.TierStandard {
		t.Errorf("Tier = %q, want standard", l.Tier())
	}
	if l.SaleLimit() != nil {
		t.Errorf("SaleLimit = %v, want nil", l.SaleLimit())
	}
	if l.PurchaseCount() != 0 {
		t.Errorf("PurchaseCount = %d, want 0", l.PurchaseCount())
	}
}

func TestGetters_GstVerified_RoundTripsFromUnmarshal(t *testing.T) {
	t.Parallel()
	snap := platformlead.Snapshot{
		ID:                     leadID,
		SourceContactID:        contactID,
		Form:                   sampleForm(t),
		GstVerified:            true,
		Tier:                   platformlead.TierStandard,
		VerifiedAt:             now,
		VerifiedByMembershipID: agentID,
		CreatedAt:              now,
	}
	l := platformlead.UnmarshalFromDB(snap)
	if !l.GstVerified() {
		t.Error("GstVerified after Unmarshal = false, want true")
	}
}

func TestPullEvents_DrainsAndClears(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	first := l.PullEvents()
	if len(first) != 1 {
		t.Fatalf("first PullEvents = %d, want 1", len(first))
	}
	second := l.PullEvents()
	if second != nil {
		t.Errorf("second PullEvents = %v, want nil", second)
	}
}

func TestComputePurchasePricePaisa(t *testing.T) {
	t.Parallel()
	base := int64(100000)
	cases := []struct {
		name        string
		prior       int
		packageBps  int
		wantAtMost  int64
		wantAtLeast int64
	}{
		{"first buyer no discount", 0, 0, base, base},
		{"second buyer 5pct off", 1, 0, 95000, 95000},
		{"volume capped at 30pct", 10, 0, 70000, 70000},
		{"package discount stacks", 1, 1000, 85000, 85000}, // 5% volume + 10% package
		{"total capped at 50pct", 20, 4000, 50000, 50000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := platformlead.ComputePurchasePricePaisa(base, tc.prior, tc.packageBps)
			if got > tc.wantAtMost || got < tc.wantAtLeast {
				t.Errorf("price = %d, want in [%d, %d]", got, tc.wantAtLeast, tc.wantAtMost)
			}
		})
	}
}
