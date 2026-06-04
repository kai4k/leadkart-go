package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// CrmReminderCreatedV1 — a Reminder was minted in a tenant. The Type
// field carries the source classification (`callback`, `mature_lead`,
// `manual`) so downstream consumers can route per source.
//
// Wire alias: `crm.reminder_created.v1`. Tenant-scoped.
type CrmReminderCreatedV1 struct {
	ReminderID             uuid.UUID `json:"reminder_id"`
	TenantIDClaim          uuid.UUID `json:"tenant_id"`
	LeadID                 uuid.UUID `json:"lead_id"`
	AssignedToMembershipID uuid.UUID `json:"assigned_to_membership_id"`
	Type                   string    `json:"type"`
	DueAtUTC               time.Time `json:"due_at_utc"`
	SourceCallLogID        uuid.UUID `json:"source_call_log_id,omitzero"`
	CreatedByMembershipID  uuid.UUID `json:"created_by_membership_id,omitzero"`
	OccurredAtUTC          time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmReminderCreatedV1) Topic() string { return "crm.reminder_created.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmReminderCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmReminderCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmReminderMarkedSentV1 — a pending reminder was marked as sent
// (user explicitly fired the reminder action).
//
// Wire alias: `crm.reminder_marked_sent.v1`.
type CrmReminderMarkedSentV1 struct {
	ReminderID           uuid.UUID `json:"reminder_id"`
	TenantIDClaim        uuid.UUID `json:"tenant_id"`
	LeadID               uuid.UUID `json:"lead_id"`
	MarkedByMembershipID uuid.UUID `json:"marked_by_membership_id"`
	OccurredAtUTC        time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmReminderMarkedSentV1) Topic() string { return "crm.reminder_marked_sent.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmReminderMarkedSentV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmReminderMarkedSentV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// CrmReminderCancelledV1 — a pending reminder was cancelled by a user.
// Reason is REQUIRED (audit doctrine).
//
// Wire alias: `crm.reminder_cancelled.v1`.
type CrmReminderCancelledV1 struct {
	ReminderID              uuid.UUID `json:"reminder_id"`
	TenantIDClaim           uuid.UUID `json:"tenant_id"`
	LeadID                  uuid.UUID `json:"lead_id"`
	CancelledByMembershipID uuid.UUID `json:"cancelled_by_membership_id"`
	Reason                  string    `json:"reason"`
	OccurredAtUTC           time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (CrmReminderCancelledV1) Topic() string { return "crm.reminder_cancelled.v1" }

// OccurredAt returns the domain timestamp.
func (e CrmReminderCancelledV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e CrmReminderCancelledV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time assertions + registry binding.
var (
	_ TenantScoped = CrmReminderCreatedV1{}
	_ TenantScoped = CrmReminderMarkedSentV1{}
	_ TenantScoped = CrmReminderCancelledV1{}

	_ = register(CrmReminderCreatedV1{})
	_ = register(CrmReminderMarkedSentV1{})
	_ = register(CrmReminderCancelledV1{})
)
