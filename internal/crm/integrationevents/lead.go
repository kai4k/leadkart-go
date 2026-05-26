package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// CrmLeadCreatedV1 — a CrmLead has been minted in a tenant. Source path
// is either the lead-purchased subscriber (SourcePurchaseID populated)
// or a manual import (SourcePurchaseID == uuid.Nil; manual path lands
// in slice 2+).
//
// Wire alias: `crm.lead_created.v1`. Tenant-scoped.
type CrmLeadCreatedV1 struct {
	LeadID                uuid.UUID `json:"lead_id"`
	TenantIDClaim         uuid.UUID `json:"tenant_id"`
	SourcePurchaseID      uuid.UUID `json:"source_purchase_id,omitzero"` // omit when manual-import path
	CreatedByMembershipID uuid.UUID `json:"created_by_membership_id,omitzero"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmLeadCreatedV1) Topic() string { return "crm.lead_created.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmLeadCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmLeadCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmLeadAssignedV1 — a CrmLead's current assignee changed (first
// assignment or reassignment). AssignmentHistory row written in the
// same transaction by the command handler.
//
// Wire alias: `crm.lead_assigned.v1`.
type CrmLeadAssignedV1 struct {
	LeadID                 uuid.UUID `json:"lead_id"`
	TenantIDClaim          uuid.UUID `json:"tenant_id"`
	PreviousAssignee       uuid.UUID `json:"previous_assignee,omitzero"` // zero on first assignment
	AssigneeMembershipID   uuid.UUID `json:"assignee_membership_id"`
	AssignedByMembershipID uuid.UUID `json:"assigned_by_membership_id"`
	OccurredAtUTC          time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmLeadAssignedV1) Topic() string { return "crm.lead_assigned.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmLeadAssignedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmLeadAssignedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmLeadStageChangedV1 — the lead's pipeline stage advanced via the
// state machine. Convert / Lose are emitted as CrmLeadConvertedV1 /
// CrmLeadLostV1; this event covers only the New→Contacted→…→Negotiation
// chain.
//
// Wire alias: `crm.lead_stage_changed.v1`.
type CrmLeadStageChangedV1 struct {
	LeadID                uuid.UUID `json:"lead_id"`
	TenantIDClaim         uuid.UUID `json:"tenant_id"`
	OldStage              string    `json:"old_stage"`
	NewStage              string    `json:"new_stage"`
	ChangedByMembershipID uuid.UUID `json:"changed_by_membership_id"`
	Reason                string    `json:"reason,omitzero"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmLeadStageChangedV1) Topic() string { return "crm.lead_stage_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmLeadStageChangedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmLeadStageChangedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmLeadTemperatureChangedV1 — the lead's qualitative interest signal
// changed. Independent of [CrmLeadStageChangedV1].
//
// Wire alias: `crm.lead_temperature_changed.v1`.
type CrmLeadTemperatureChangedV1 struct {
	LeadID                uuid.UUID `json:"lead_id"`
	TenantIDClaim         uuid.UUID `json:"tenant_id"`
	OldTemperature        string    `json:"old_temperature"`
	NewTemperature        string    `json:"new_temperature"`
	ChangedByMembershipID uuid.UUID `json:"changed_by_membership_id"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmLeadTemperatureChangedV1) Topic() string { return "crm.lead_temperature_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmLeadTemperatureChangedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmLeadTemperatureChangedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmCallLoggedV1 — a CallLog was created against a lead. Wire alias:
// `crm.call_logged.v1`. Append-only — no subsequent update / delete events.
type CrmCallLoggedV1 struct {
	CallID               uuid.UUID `json:"call_id"`
	LeadID               uuid.UUID `json:"lead_id"`
	TenantIDClaim        uuid.UUID `json:"tenant_id"`
	Outcome              string    `json:"outcome"`
	LoggedByMembershipID uuid.UUID `json:"logged_by_membership_id"`
	OccurredAtUTC        time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmCallLoggedV1) Topic() string { return "crm.call_logged.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmCallLoggedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmCallLoggedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmLeadConvertedV1 — terminal-success transition. The future Orders
// module consumes this as the trigger to seed a Quotation skeleton per
// ADR 0060 contract for the v0.4+ Orders work.
//
// Wire alias: `crm.lead_converted.v1`.
type CrmLeadConvertedV1 struct {
	LeadID                  uuid.UUID `json:"lead_id"`
	TenantIDClaim           uuid.UUID `json:"tenant_id"`
	ConvertedByMembershipID uuid.UUID `json:"converted_by_membership_id"`
	OccurredAtUTC           time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmLeadConvertedV1) Topic() string { return "crm.lead_converted.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmLeadConvertedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmLeadConvertedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmLeadLostV1 — terminal-failure transition. Carries the audit
// reason required at [crmlead.CrmLead.Lose].
//
// Wire alias: `crm.lead_lost.v1`.
type CrmLeadLostV1 struct {
	LeadID             uuid.UUID `json:"lead_id"`
	TenantIDClaim      uuid.UUID `json:"tenant_id"`
	LostByMembershipID uuid.UUID `json:"lost_by_membership_id"`
	Reason             string    `json:"reason"`
	OccurredAtUTC      time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmLeadLostV1) Topic() string { return "crm.lead_lost.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmLeadLostV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmLeadLostV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time assertions: each CRM event satisfies TenantScoped +
// Event. Build fails if a future field-rename or method drop breaks
// the contract.
var (
	_ TenantScoped = CrmLeadCreatedV1{}
	_ TenantScoped = CrmLeadAssignedV1{}
	_ TenantScoped = CrmLeadStageChangedV1{}
	_ TenantScoped = CrmLeadTemperatureChangedV1{}
	_ TenantScoped = CrmCallLoggedV1{}
	_ TenantScoped = CrmLeadConvertedV1{}
	_ TenantScoped = CrmLeadLostV1{}

	_ = register(CrmLeadCreatedV1{})
	_ = register(CrmLeadAssignedV1{})
	_ = register(CrmLeadStageChangedV1{})
	_ = register(CrmLeadTemperatureChangedV1{})
	_ = register(CrmCallLoggedV1{})
	_ = register(CrmLeadConvertedV1{})
	_ = register(CrmLeadLostV1{})
)
