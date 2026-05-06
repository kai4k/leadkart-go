package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// TenantRegisteredV1 — a tenant has been registered. Consumed by
// Platform (initialise lead credits), CRM (seed default pipeline),
// Notifications (welcome email).
//
// Platform-scoped: registration creates the tenant; there's no
// "current tenant" context separate from the new tenant_id. Per
// .NET `messaging.md` "Identity (Tenant — PlatformEvent)" classification.
//
// Carries TENANT-AGGREGATE fields only. Cross-module consumers that
// need admin Person info (e.g. Notifications welcome-email) look up
// via TenantID; sibling integration events (PersonCreatedV1 +
// MembershipCreatedV1) carry the admin identities for direct routing.
type TenantRegisteredV1 struct {
	platformMarker

	TenantID      uuid.UUID `json:"tenant_id"`
	Slug          string    `json:"slug"`
	LegalName     string    `json:"legal_name"`
	DisplayName   string    `json:"display_name"`
	AdminEmail    string    `json:"admin_email"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
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

// TenantProfileUpdatedV1 — tenant changed its display fields
// (LegalName + DisplayName). Consumed by Notifications + any
// integration cache that materialises tenant display strings.
//
// Other tenant attributes (admin email, statutory IDs, settings) ride
// dedicated events per the .NET parent's vocabulary split.
type TenantProfileUpdatedV1 struct {
	platformMarker

	TenantID       uuid.UUID `json:"tenant_id"`
	OldLegalName   string    `json:"old_legal_name"`
	OldDisplayName string    `json:"old_display_name"`
	NewLegalName   string    `json:"new_legal_name"`
	NewDisplayName string    `json:"new_display_name"`
	OccurredAtUTC  time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantProfileUpdatedV1) Topic() string { return "identity.tenant_profile_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantProfileUpdatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

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

// TenantStatutoryUpdatedV1 — tenant changed its declared Indian
// statutory IDs (GST/PAN/DrugLicence). Subscribers (audit, search-
// index reindex, compliance reporting) consume the OLD/NEW pair to
// render diffs.
//
// Empty-string fields mean "not declared" — first-time declaration
// has empty old_*; full clear has empty new_*.
type TenantStatutoryUpdatedV1 struct {
	platformMarker

	TenantID         uuid.UUID `json:"tenant_id"`
	OldGST           string    `json:"old_gst"`
	OldPAN           string    `json:"old_pan"`
	OldDrugLicence   string    `json:"old_drug_licence"`
	NewGST           string    `json:"new_gst"`
	NewPAN           string    `json:"new_pan"`
	NewDrugLicence   string    `json:"new_drug_licence"`
	OccurredAtUTC    time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantStatutoryUpdatedV1) Topic() string { return "identity.tenant_statutory_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantStatutoryUpdatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantMarkedForDeletionV1 — operator entered the 30-day grace window
// per `data-retention.md` "Tenant deletion saga". Subscribers MAY
// block tenant ops immediately or wait for terminal TenantDeletedV1.
type TenantMarkedForDeletionV1 struct {
	platformMarker

	TenantID         uuid.UUID `json:"tenant_id"`
	Reason           string    `json:"reason"`
	ScheduledAtUTC   time.Time `json:"scheduled_at_utc"`
	OccurredAtUTC    time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantMarkedForDeletionV1) Topic() string { return "identity.tenant_marked_for_deletion.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantMarkedForDeletionV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantRestoredV1 — pending deletion cancelled within the grace
// window. Subscribers that blocked ops on TenantMarkedForDeletionV1
// re-enable.
type TenantRestoredV1 struct {
	platformMarker

	TenantID      uuid.UUID `json:"tenant_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantRestoredV1) Topic() string { return "identity.tenant_restored.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantRestoredV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantDeletedV1 — terminal hard-delete event. Subscribers SHOULD
// anonymise remaining PII per their module's classification (CRM
// lead notes, Tasks comments). Audit log retained 7 years per SOC2
// CC4.1 / DPDP §12; tenant row retained for FK integrity.
type TenantDeletedV1 struct {
	platformMarker

	TenantID      uuid.UUID `json:"tenant_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (TenantDeletedV1) Topic() string { return "identity.tenant_deleted.v1" }

// OccurredAt returns the domain timestamp.
func (e TenantDeletedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// Compile-time assertions: each Tenant-aggregate event is Platform +
// Event. Build fails if a future field-rename or method drop breaks
// the contract.
var (
	_ Platform = TenantRegisteredV1{}
	_ Platform = TenantActivatedV1{}
	_ Platform = TenantProfileUpdatedV1{}
	_ Platform = TenantStatutoryUpdatedV1{}
	_ Platform = TenantSuspendedV1{}
	_ Platform = TenantMarkedForDeletionV1{}
	_ Platform = TenantRestoredV1{}
	_ Platform = TenantDeletedV1{}

	_ = register(TenantRegisteredV1{})
	_ = register(TenantActivatedV1{})
	_ = register(TenantProfileUpdatedV1{})
	_ = register(TenantStatutoryUpdatedV1{})
	_ = register(TenantSuspendedV1{})
	_ = register(TenantMarkedForDeletionV1{})
	_ = register(TenantRestoredV1{})
	_ = register(TenantDeletedV1{})
)
