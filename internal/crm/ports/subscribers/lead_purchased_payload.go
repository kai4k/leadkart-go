// Package subscribers holds CRM-side Watermill subscriber handlers.
package subscribers

// arch-test:idempotency-via-wire-shape-only — DTO + topic constant only; no handler logic, nothing to dedup.

import (
	"time"

	platformevents "github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// LeadPurchasedV1 is the wire shape CRM decodes from the lead-purchase
// event. IDs are JSON strings (canonical RFC 4122 UUIDs) so the payload
// round-trips byte-for-byte without a uuid codec.
type LeadPurchasedV1 struct {
	PurchaseID              string         `json:"purchase_id"`
	TenantID                string         `json:"tenant_id"`
	PlatformLeadID          string         `json:"platform_lead_id"`
	PurchasedAt             time.Time      `json:"purchased_at"`
	PurchasedByMembershipID string         `json:"purchased_by_membership_id"`
	AmountPaisa             int64          `json:"amount_paisa"`
	LeadSnapshot            LeadSnapshotV1 `json:"lead_snapshot"`
}

// LeadSnapshotV1 mirrors the nested `lead_snapshot` payload — every
// BRD §5 lead-form field flattened to wire-stable primitives.
type LeadSnapshotV1 struct {
	ContactName    string   `json:"contact_name"`
	MobileE164     string   `json:"mobile_e164"`
	Email          string   `json:"email"`
	PinCode        string   `json:"pin_code"`
	City           string   `json:"city"`
	District       string   `json:"district"`
	State          string   `json:"state"`
	Street         string   `json:"street"`
	HasDrugLicence bool     `json:"has_drug_licence"`
	HasGst         bool     `json:"has_gst"`
	GstNumber      string   `json:"gst_number"`
	HasPan         bool     `json:"has_pan"`
	PanNumber      string   `json:"pan_number"`
	BusinessType   string   `json:"business_type"`
	MedicineSystem string   `json:"medicine_system"`
	ProductRanges  []string `json:"product_ranges"`
	DosageForms    []string `json:"dosage_forms"`
	OrderValue     string   `json:"order_value"`
	BuyTimeline    string   `json:"buy_timeline"`
}

// LeadPurchasedTopic is the event_type the subscriber filters on. It
// aliases the producer's exported constant so the producer and consumer
// share one source of truth and cannot drift.
const LeadPurchasedTopic = platformevents.TopicLeadPurchasedV1
