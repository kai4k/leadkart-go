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
	leadID    = platformlead.ID("01900000-0000-7000-8000-000000000020")
	contactID = unverifiedcontact.ID("01900000-0000-7000-8000-000000000010")
	tenantA   = platformlead.TenantID("01900000-0000-7000-8000-000000000200")
	tenantB   = platformlead.TenantID("01900000-0000-7000-8000-000000000201")
	agentID   = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000001")
	memberA   = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000301")
	memberB   = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000302")
	now       = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
)

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
	if !l.IsAvailable() {
		t.Error("expected unsold lead to be available")
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

func TestPurchase_HappyPath(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	_ = l.PullEvents()
	purchasedAt := now.Add(time.Hour)
	if err := l.Purchase(tenantA, memberA, 50000, purchasedAt); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if l.IsAvailable() {
		t.Error("expected lead to be sold")
	}
	if l.SoldToTenantID() != tenantA {
		t.Errorf("SoldToTenantID=%q", l.SoldToTenantID())
	}
	if l.AmountPaisa() != 50000 {
		t.Errorf("AmountPaisa=%d", l.AmountPaisa())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	pe, ok := evs[0].(platformlead.PurchasedEvent)
	if !ok {
		t.Fatalf("expected PurchasedEvent, got %T", evs[0])
	}
	if pe.TenantID != tenantA || pe.AmountPaisa != 50000 {
		t.Errorf("event=%+v", pe)
	}
}

func TestPurchase_RejectsZeroOrNegativeAmount(t *testing.T) {
	t.Parallel()
	for _, amt := range []int64{0, -1, -100} {
		l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
		err := l.Purchase(tenantA, memberA, amt, now.Add(time.Hour))
		if !errors.Is(err, platformlead.ErrInvalid) {
			t.Errorf("amount %d: expected ErrInvalid, got %v", amt, err)
		}
	}
}

func TestPurchase_AlreadySoldByOther_Rejected(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	_ = l.Purchase(tenantA, memberA, 50000, now.Add(time.Hour))
	err := l.Purchase(tenantB, memberB, 50000, now.Add(2*time.Hour))
	if !errors.Is(err, platformlead.ErrAlreadySold) {
		t.Errorf("expected ErrAlreadySold, got %v", err)
	}
}

func TestPurchase_AlreadySoldBySameTenantSamePrice_Idempotent(t *testing.T) {
	t.Parallel()
	l, _ := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, now)
	_ = l.Purchase(tenantA, memberA, 50000, now.Add(time.Hour))
	_ = l.PullEvents()
	if err := l.Purchase(tenantA, memberA, 50000, now.Add(2*time.Hour)); err != nil {
		t.Errorf("idempotent expected, got %v", err)
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Errorf("idempotent retry should emit no events, got %d", len(evs))
	}
}

func TestUnmarshalFromDB_RoundTrip(t *testing.T) {
	t.Parallel()
	snap := platformlead.Snapshot{
		ID:                     leadID,
		SourceContactID:        contactID,
		Form:                   sampleForm(t),
		SoldToTenantID:         tenantA,
		SoldAt:                 now.Add(time.Hour),
		SoldToMembershipID:     memberA,
		AmountPaisa:            50000,
		VerifiedAt:             now,
		VerifiedByMembershipID: agentID,
		CreatedAt:              now,
	}
	l := platformlead.UnmarshalFromDB(snap)
	if l.IsAvailable() {
		t.Error("expected loaded lead to be sold")
	}
	if l.AmountPaisa() != 50000 {
		t.Errorf("AmountPaisa round-trip failed")
	}
}
