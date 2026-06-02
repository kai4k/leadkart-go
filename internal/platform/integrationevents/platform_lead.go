package integrationevents

import (
	"time"
)

// LeadVerifiedV1 signals an UnverifiedContact graduated to a marketplace
// PlatformLead. Platform-scoped; no Slice 1 consumer (reserved for analytics
// and a future Notifications subscriber).
//
// Wire shape frozen per ADR 0059; UUIDs are strings. Carries the full
// LeadSnapshot for consumer autonomy (Udi Dahan rule).
type LeadVerifiedV1 struct {
	platformMarker

	PlatformLeadID         string       `json:"platform_lead_id"`
	VerifiedAt             time.Time    `json:"verified_at"`
	VerifiedByMembershipID string       `json:"verified_by_membership_id"`
	LeadSnapshot           LeadSnapshot `json:"lead_snapshot"`
}

// Topic returns the wire alias.
func (LeadVerifiedV1) Topic() string { return "platform.lead_verified.v1" }

// OccurredAt returns the domain timestamp.
func (e LeadVerifiedV1) OccurredAt() time.Time { return e.VerifiedAt }

var (
	_ Platform = LeadVerifiedV1{}
	_          = register(LeadVerifiedV1{})
)

// LeadPurchasedV1 signals a tenant purchased a PlatformLead. TenantScoped to
// the buyer. CRM (Phase 2.2) consumes it to build a CrmLead from the snapshot.
//
// UUID fields are wire-shaped strings per ADR 0059. AmountPaisa is INR paise —
// integer minor units, never float for money (coding-standards.md).
type LeadPurchasedV1 struct {
	PurchaseID              string       `json:"purchase_id"`
	TenantID                string       `json:"tenant_id"`
	PlatformLeadID          string       `json:"platform_lead_id"`
	PurchasedAt             time.Time    `json:"purchased_at"`
	PurchasedByMembershipID string       `json:"purchased_by_membership_id"`
	AmountPaisa             int64        `json:"amount_paisa"`
	LeadSnapshot            LeadSnapshot `json:"lead_snapshot"`
}

// TopicLeadPurchasedV1 is the wire alias for the purchase event. Single source
// of truth: the producer stamps it on the outbox row and CRM's subscriber
// filters on the same constant, so the two cannot drift (prior bug: an
// underscore/hyphen mismatch silently dropped this flow).
const TopicLeadPurchasedV1 = "platform.lead_purchased.v1"

// Topic returns the wire alias.
func (LeadPurchasedV1) Topic() string { return TopicLeadPurchasedV1 }

// OccurredAt returns the domain timestamp.
func (e LeadPurchasedV1) OccurredAt() time.Time { return e.PurchasedAt }

// TenantIDString satisfies [TenantScoped] — surfaces tenant_id for
// envelope-level routing (messaging.md "Tenant channel").
func (e LeadPurchasedV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = LeadPurchasedV1{}
	_              = register(LeadPurchasedV1{})
)
