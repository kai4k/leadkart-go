package integrationevents

import (
	"time"
)

// LeadVerifiedV1 — an UnverifiedContact graduated to a PlatformLead in
// the marketplace. Platform-scoped (no consumer in Slice 1; reserved
// for analytics in Slice 2 + future Notifications "new lead in your
// product range" subscriber).
//
// Wire shape frozen per ADR 0059. All UUIDs are wire-shaped as strings
// (Stripe / Auth0 canon: cross-language consumers don't need a uuid
// codec). Includes the full LeadSnapshot for consumer autonomy
// (Udi Dahan rule).
type LeadVerifiedV1 struct {
	platformMarker

	PlatformLeadID         string       `json:"platform_lead_id"`
	VerifiedAt             time.Time    `json:"verified_at"`
	VerifiedByMembershipID string       `json:"verified_by_membership_id"`
	LeadSnapshot           LeadSnapshot `json:"lead_snapshot"`
}

// Topic returns the canonical wire alias.
func (LeadVerifiedV1) Topic() string { return "platform.lead_verified.v1" }

// OccurredAt returns the domain timestamp.
func (e LeadVerifiedV1) OccurredAt() time.Time { return e.VerifiedAt }

var (
	_ Platform = LeadVerifiedV1{}
	_          = register(LeadVerifiedV1{})
)

// LeadPurchasedV1 — a tenant purchased a PlatformLead. TenantScoped:
// the purchasing tenant. Consumed by CRM (Phase 2.2) to create a
// CrmLead aggregate from the snapshot.
//
// All UUID fields are wire-shaped as strings (CLAUDE.md slice-1 frozen
// brief — matches CRM subscriber's local mirror without per-language
// uuid codec). AmountPaisa is INR paise (NEVER float for money — per
// `coding-standards.md` "Money: integer minor units, not float").
type LeadPurchasedV1 struct {
	PurchaseID              string       `json:"purchase_id"`
	TenantID                string       `json:"tenant_id"`
	PlatformLeadID          string       `json:"platform_lead_id"`
	PurchasedAt             time.Time    `json:"purchased_at"`
	PurchasedByMembershipID string       `json:"purchased_by_membership_id"`
	AmountPaisa             int64        `json:"amount_paisa"`
	LeadSnapshot            LeadSnapshot `json:"lead_snapshot"`
}

// TopicLeadPurchasedV1 is the canonical wire alias for the lead-purchase
// event. Single source of truth: the producer stamps it on the outbox row
// and CRM's subscriber filters on this same constant, so the two cannot
// drift (the underscore/hyphen mismatch that silently dropped this flow).
const TopicLeadPurchasedV1 = "platform.lead_purchased.v1"

// Topic returns the canonical wire alias.
func (LeadPurchasedV1) Topic() string { return TopicLeadPurchasedV1 }

// OccurredAt returns the domain timestamp.
func (e LeadPurchasedV1) OccurredAt() time.Time { return e.PurchasedAt }

// TenantIDString satisfies [TenantScoped] — surfaces the tenant field
// for envelope-level routing per `messaging.md` "Tenant channel". The
// wire field is `tenant_id` (string); this accessor preserves type-
// agnostic access for the routing pipeline.
func (e LeadPurchasedV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = LeadPurchasedV1{}
	_              = register(LeadPurchasedV1{})
)
