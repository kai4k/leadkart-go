// Package subscribers holds CRM-side Watermill subscriber handlers.
//
// CROSS-MODULE EVENT MIRROR: the LeadPurchasedV1 struct in this file is a
// LOCAL MIRROR of the platform.lead-purchased.v1 wire contract. The
// Platform module ships the canonical struct in
// internal/platform/integrationevents/ but is being built in parallel.
// Once both branches merge, this file's struct will be REPLACED with an
// import from the platform package; the wire payload (JSON) is
// identical so consumers don't need rewiring.
//
// PER ADR 0001 modular-monolith canon: cross-module communication is
// via integration events on the bus, NEVER direct package imports of
// another module's domain/app/ports/adapters. The integrationevents
// package is the ONE exception (it carries framework-neutral wire
// records the publisher considers public).
//
// Wire alias: `platform.lead-purchased.v1` (frozen by the slice
// brief). Field shape exactly matches the brief's struct definition.
package subscribers

import (
	"time"
)

// LeadPurchasedV1 mirrors `platform.lead-purchased.v1` per the slice
// brief. Tenant-scoped. Replace this struct with the Platform-module
// import when both branches merge.
//
// FIELD-SHAPE CONTRACT: every field type matches the frozen brief
// VERBATIM. TenantID is a JSON STRING (not uuid.UUID), because the
// brief specifies a UUID encoded as RFC 4122 lowercase canonical form,
// which JSON-decodes into Go as `string`. Earlier drafts decoded into
// uuid.UUID then stringified at the boundary — works at runtime but
// breaks the round-trip equality the contract demands (uuid.UUID
// re-encodes WITHOUT braces / hyphens preserved as input, and
// stripping/rewriting a wire-stable field is a CI smell). All other
// IDs (PurchaseID, PlatformLeadID, PurchasedByMembershipID) are
// already string per the brief.
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

// LeadPurchasedTopic is the canonical wire-alias the Watermill
// subscriber filters on (message.Metadata.Get(messaging.HeaderEventType)).
const LeadPurchasedTopic = "platform.lead-purchased.v1"
