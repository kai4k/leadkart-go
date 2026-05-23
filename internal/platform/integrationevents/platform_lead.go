package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// LeadVerifiedV1 — an UnverifiedContact graduated to a PlatformLead in
// the marketplace. Platform-scoped (no consumer in Slice 1; reserved
// for analytics in Slice 2 + future Notifications "new lead in your
// product range" subscriber).
//
// Wire shape frozen per ADR 0059. Includes the full LeadSnapshot for
// consumer autonomy (Udi Dahan rule).
type LeadVerifiedV1 struct {
	platformMarker

	PlatformLeadID         uuid.UUID    `json:"platform_lead_id"`
	VerifiedAt             time.Time    `json:"verified_at"`
	VerifiedByMembershipID uuid.UUID    `json:"verified_by_membership_id"`
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
// AmountPaisa is INR paise (NEVER float for money — per `coding-
// standards.md` "Money: integer minor units, not float").
type LeadPurchasedV1 struct {
	PurchaseID              uuid.UUID    `json:"purchase_id"`
	TenantIDValue           uuid.UUID    `json:"tenant_id"`
	PlatformLeadID          uuid.UUID    `json:"platform_lead_id"`
	PurchasedAt             time.Time    `json:"purchased_at"`
	PurchasedByMembershipID uuid.UUID    `json:"purchased_by_membership_id"`
	AmountPaisa             int64        `json:"amount_paisa"`
	LeadSnapshot            LeadSnapshot `json:"lead_snapshot"`
}

// Topic returns the canonical wire alias.
func (LeadPurchasedV1) Topic() string { return "platform.lead_purchased.v1" }

// OccurredAt returns the domain timestamp.
func (e LeadPurchasedV1) OccurredAt() time.Time { return e.PurchasedAt }

// TenantID satisfies [TenantScoped] — surfaces the tenant field for
// envelope-level routing per `messaging.md` "Tenant channel".
func (e LeadPurchasedV1) TenantID() uuid.UUID { return e.TenantIDValue }

var (
	_ TenantScoped = LeadPurchasedV1{}
	_              = register(LeadPurchasedV1{})
)
