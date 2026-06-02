package subscribers_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/crm/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	platformevents "github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// silentLog returns a no-output *slog.Logger for tests — required by
// subscriber constructors per Mat Ryer canon (no nil-fallback).
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Subscriber tests use the canonical co-located crmleadtest.FakeRepository
// (per-aggregate fake mandate) rather than a hand-rolled local fake — the
// faithful fake errors on a source_purchase_id collision, so a regression
// that broke the handler's at-most-once short-circuit would surface here.

func validEvent(tenantID string, purchase string) platformevents.LeadPurchasedV1 {
	return platformevents.LeadPurchasedV1{
		PurchaseID:              purchase,
		TenantID:                tenantID,
		PlatformLeadID:          uuid.NewString(),
		PurchasedAt:             time.Now().UTC(), // arch-test:wall-clock -- wire-envelope fixture; subscriber doesn't pin timestamps.
		PurchasedByMembershipID: uuid.NewString(),
		AmountPaisa:             50000,
		LeadSnapshot: platformevents.LeadSnapshot{
			ContactName:    "Test Pharma",
			MobileE164:     "+919812345678",
			Email:          "x@example.com",
			PinCode:        "411001",
			City:           "Pune",
			District:       "Pune",
			State:          "Maharashtra",
			HasDrugLicence: true,
			HasGst:         true,
			BusinessType:   "PCD",
			MedicineSystem: "Allopathic",
			OrderValue:     "Upto25000",
			BuyTimeline:    "WithinWeek",
		},
	}
}

// Post-cqrs (ADR 0067): the handler receives the already-decoded typed
// event; topic routing + payload decode are the EventProcessor's job, so
// the old wrong-topic + malformed-payload unit cases are gone.

func TestPurchasedLeadIngestor_HappyPath(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	h := subscribers.NewPurchasedLeadIngestor(command.NewIngestPurchasedLeadHandler(leads, time.Now, func() crmlead.ID { return crmlead.ID(uuid.NewString()) }), silentLog())
	tenantID := uuid.NewString()
	purchase := uuid.NewString()
	evt := validEvent(tenantID, purchase)
	if err := h.Handle(t.Context(), &evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := leads.GetBySourcePurchaseID(t.Context(), tenant.ID(tenantID), purchase)
	if err != nil {
		t.Fatalf("GetBySourcePurchaseID: %v", err)
	}
	if got.TenantID().String() != tenantID {
		t.Fatalf("tenant: %q", got.TenantID())
	}
}

func TestPurchasedLeadIngestor_IdempotentOnReplay(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	h := subscribers.NewPurchasedLeadIngestor(command.NewIngestPurchasedLeadHandler(leads, time.Now, func() crmlead.ID { return crmlead.ID(uuid.NewString()) }), silentLog())
	tenantID := uuid.NewString()
	purchase := uuid.NewString()
	evt := validEvent(tenantID, purchase)

	if err := h.Handle(t.Context(), &evt); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := h.Handle(t.Context(), &evt); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Still ONE lead.
	if len(leads.ByPurchase) != 1 {
		t.Fatalf("ByPurchase entries: %d", len(leads.ByPurchase))
	}
}

// TestLeadPurchasedV1_FrozenWireContract pins the field types of the
// LeadPurchasedV1 mirror against the brief's canonical JSON shape.
//
// The fixture below IS the canonical wire payload — every field type
// (string IDs, RFC3339 timestamp, int64 paisa) reflects what the
// Platform publisher will produce. The test asserts our mirror decodes
// every field WITHOUT any in-flight type coercion and re-encodes to
// byte-equal canonical JSON. Drift in either direction (string→uuid,
// missing field, renamed json tag) fails the round-trip equality
// check.
//
// Earlier draft had TenantID typed uuid.UUID — works at runtime but
// re-encodes through Go's uuid.MarshalJSON which is a different code
// path from the publisher's strconv-style string emit. A defensive
// contract test prevents that drift class.
func TestLeadPurchasedV1_FrozenWireContract(t *testing.T) {
	t.Parallel()

	// Canonical wire payload per the brief. Field order must match the
	// struct's json tag order so re-encode produces byte-equal output.
	const canonical = `{` +
		`"purchase_id":"01HN8ZN3X4Y5MN0PQR7VWXY8Z3",` +
		`"tenant_id":"7f7b0e6a-3c52-4f25-9c1d-7e8f44b1c001",` +
		`"platform_lead_id":"01HN8ZN3X4Y5MN0PQR7VWXY8Z9",` +
		`"purchased_at":"2026-06-02T12:00:00Z",` +
		`"purchased_by_membership_id":"01HN8ZN3X4Y5MN0PQR7VWXY8Z4",` +
		`"amount_paisa":50000,` +
		`"lead_snapshot":{` +
		`"contact_name":"Test Pharma",` +
		`"mobile_e164":"+919812345678",` +
		`"email":"x@example.com",` +
		`"pin_code":"411001",` +
		`"city":"Pune",` +
		`"district":"Pune",` +
		`"state":"Maharashtra",` +
		`"street":"",` +
		`"has_drug_licence":true,` +
		`"has_gst":true,` +
		`"gst_number":"",` +
		`"has_pan":false,` +
		`"pan_number":"",` +
		`"business_type":"PCD",` +
		`"medicine_system":"Allopathic",` +
		`"product_ranges":["Antibiotic","Cardiac"],` +
		`"dosage_forms":["Tablet","Syrup"],` +
		`"order_value":"Upto25000",` +
		`"buy_timeline":"WithinWeek"` +
		`}}`

	var got platformevents.LeadPurchasedV1
	if err := json.Unmarshal([]byte(canonical), &got); err != nil {
		t.Fatalf("decode canonical: %v", err)
	}

	if got.TenantID != "7f7b0e6a-3c52-4f25-9c1d-7e8f44b1c001" {
		t.Fatalf("TenantID drift: got %q", got.TenantID)
	}
	if got.PurchaseID != "01HN8ZN3X4Y5MN0PQR7VWXY8Z3" {
		t.Fatalf("PurchaseID drift: got %q", got.PurchaseID)
	}
	if got.AmountPaisa != 50000 {
		t.Fatalf("AmountPaisa drift: got %d", got.AmountPaisa)
	}

	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(out) != canonical {
		t.Fatalf("round-trip drift:\n got: %s\nwant: %s", string(out), canonical)
	}
}
