package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// TenantRegisteredV1 — a tenant + its admin Person + admin Membership
// have been created. Consumed by Platform (initialise lead credits),
// CRM (seed default pipeline), Notifications (welcome email).
//
// Platform-scoped: registration creates the tenant; there's no
// "current tenant" context separate from the new tenant_id. Per
// .NET `messaging.md` "Identity (Tenant — PlatformEvent)" classification.
type TenantRegisteredV1 struct {
	platformMarker

	TenantID         uuid.UUID `json:"tenant_id"`
	Slug             string    `json:"slug"`
	LegalName        string    `json:"legal_name"`
	DisplayName      string    `json:"display_name"`
	AdminEmail       string    `json:"admin_email"`
	AdminPersonID    uuid.UUID `json:"admin_person_id"`
	AdminMembershipID uuid.UUID `json:"admin_membership_id"`
	OccurredAtUTC    time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantRegisteredV1) Topic() string { return "identity.tenant_registered.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantRegisteredV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantActivatedV1 — operator transitioned a tenant from Pending or
// Suspended to Active. Consumed by CRM (re-enable lead operations) +
// any module that gated work on `Tenant.Status`.
type TenantActivatedV1 struct {
	platformMarker

	TenantID      uuid.UUID `json:"tenant_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantActivatedV1) Topic() string { return "identity.tenant_activated.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantActivatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantSuspendedV1 — operator suspended a tenant (payment overdue,
// admin action). Consumed by every module that should block ops:
// CRM stops lead assignments, Orders rejects new orders, etc.
type TenantSuspendedV1 struct {
	platformMarker

	TenantID      uuid.UUID `json:"tenant_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantSuspendedV1) Topic() string { return "identity.tenant_suspended.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantSuspendedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// Compile-time assertions: each Tenant-aggregate event is Platform +
// Event. Build fails if a future field-rename or method drop breaks
// the contract.
var (
	_ Platform = TenantRegisteredV1{}
	_ Platform = TenantActivatedV1{}
	_ Platform = TenantSuspendedV1{}

	_ = register(TenantRegisteredV1{})
	_ = register(TenantActivatedV1{})
	_ = register(TenantSuspendedV1{})
)
