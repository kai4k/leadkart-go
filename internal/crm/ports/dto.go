// Package ports holds the CRM module's inbound HTTP + subscriber
// adapters per ADR 0002 + the TDL hexagonal terminology canon.
//
// HTTP handlers translate request/response shapes into [app.Application]
// command + query calls. Per ADR 0046 the wire shapes here are
// MIRRORED in api/openapi.yaml — adding a route or field requires a
// paired spec update (CI-gated by the route-spec-drift test below).
package ports

import "time"

// ----- Lead read DTOs --------------------------------------------------------

// LeadDto is the wire shape returned by GET /api/v1/crm/leads/{leadId}
// + every item in the GET /api/v1/crm/leads list.
type LeadDto struct {
	ID                       string         `json:"id"`
	TenantID                 string         `json:"tenant_id"`
	Stage                    string         `json:"stage"`
	Temperature              string         `json:"temperature"`
	ContactName              string         `json:"contact_name"`
	PhoneE164                string         `json:"phone_e164"`
	City                     string         `json:"city,omitzero"`
	District                 string         `json:"district,omitzero"`
	State                    string         `json:"state,omitzero"`
	Pincode                  string         `json:"pincode,omitzero"`
	BusinessType             string         `json:"business_type,omitzero"`
	MedicineSystem           string         `json:"medicine_system,omitzero"`
	OrderValue               string         `json:"order_value,omitzero"`
	BuyTimeline              string         `json:"buy_timeline,omitzero"`
	HasDrugLicence           bool           `json:"has_drug_licence"`
	HasGst                   bool           `json:"has_gst"`
	GstVerified              bool           `json:"gst_verified"`
	ProductRanges            []string       `json:"product_ranges,omitzero"`
	DosageForms              []string       `json:"dosage_forms,omitzero"`
	ExtraProfile             map[string]any `json:"extra_profile,omitzero"`
	AssigneeMembershipID     string         `json:"assignee_membership_id,omitzero"`
	AssignedAt               time.Time      `json:"assigned_at,omitzero"`
	SourcePurchaseID         string         `json:"source_purchase_id,omitzero"`
	SourcePlatformLeadID     string         `json:"source_platform_lead_id,omitzero"`
	ConvertedAt              time.Time      `json:"converted_at,omitzero"`
	ConvertedByMembershipID  string         `json:"converted_by_membership_id,omitzero"`
	LostAt                   time.Time      `json:"lost_at,omitzero"`
	LostByMembershipID       string         `json:"lost_by_membership_id,omitzero"`
	LostReason               string         `json:"lost_reason,omitzero"`
	CreatedAt                time.Time      `json:"created_at"`
	CreatedByMembershipID    string         `json:"created_by_membership_id,omitzero"`
}

// ListLeadsResponse is the wire shape returned by GET /api/v1/crm/leads.
// Mirrors the [pagination.Page] shape per ADR 0038.
type ListLeadsResponse struct {
	Items      []LeadDto `json:"items"`
	HasMore    bool      `json:"has_more"`
	NextCursor string    `json:"next_cursor,omitzero"`
}

// ----- Mutation requests + responses ----------------------------------------

// AssignLeadRequest is the body for POST /api/v1/crm/leads/{leadId}/assign.
type AssignLeadRequest struct {
	AssigneeMembershipID string `json:"assignee_membership_id"`
	Reason               string `json:"reason,omitzero"`
}

// AssignLeadResponse returns the audit-history row ID + the updated
// assignee mirror.
type AssignLeadResponse struct {
	AssignmentID         string `json:"assignment_id,omitzero"`
	AssigneeMembershipID string `json:"assignee_membership_id"`
}

// ChangeStageRequest is the body for POST /api/v1/crm/leads/{leadId}/stage.
type ChangeStageRequest struct {
	NewStage string `json:"new_stage"`
	Reason   string `json:"reason,omitzero"`
}

// ChangeTemperatureRequest is the body for POST /api/v1/crm/leads/{leadId}/temperature.
type ChangeTemperatureRequest struct {
	NewTemperature string `json:"new_temperature"`
}

// LogCallRequest is the body for POST /api/v1/crm/leads/{leadId}/calls.
type LogCallRequest struct {
	Outcome string `json:"outcome"`
	Notes   string `json:"notes,omitzero"`
}

// LogCallResponse returns the new call-log ID.
type LogCallResponse struct {
	CallID string `json:"call_id"`
}

// LoseLeadRequest is the body for POST /api/v1/crm/leads/{leadId}/lose.
type LoseLeadRequest struct {
	Reason string `json:"reason"`
}

// ----- Reminder DTOs --------------------------------------------------------

// ReminderDto is the wire shape returned by GET /api/v1/crm/reminders
// items + the reminder mutation endpoints' 200/201 paths.
type ReminderDto struct {
	ID                       string    `json:"id"`
	TenantID                 string    `json:"tenant_id"`
	LeadID                   string    `json:"lead_id"`
	AssignedToMembershipID   string    `json:"assigned_to_membership_id"`
	CreatedByMembershipID    string    `json:"created_by_membership_id,omitzero"`
	SourceCallLogID          string    `json:"source_call_log_id,omitzero"`
	Type                     string    `json:"type"`
	State                    string    `json:"state"`
	DueAt                    time.Time `json:"due_at"`
	Notes                    string    `json:"notes,omitzero"`
	SentAt                   time.Time `json:"sent_at,omitzero"`
	MarkedSentByMembershipID string    `json:"marked_sent_by_membership_id,omitzero"`
	CancelledAt              time.Time `json:"cancelled_at,omitzero"`
	CancelledByMembershipID  string    `json:"cancelled_by_membership_id,omitzero"`
	CancelReason             string    `json:"cancel_reason,omitzero"`
	CreatedAt                time.Time `json:"created_at"`
}

// ListRemindersResponse is the cursor-paginated reminder page shape.
type ListRemindersResponse struct {
	Items      []ReminderDto `json:"items"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor,omitzero"`
}

// CreateReminderRequest is the body for POST /api/v1/crm/leads/{leadId}/reminders.
//
// `assigned_to_membership_id` is REQUIRED. `due_at` is REQUIRED, an
// RFC 3339 timestamp. `notes` is optional + free-text.
type CreateReminderRequest struct {
	AssignedToMembershipID string    `json:"assigned_to_membership_id"`
	DueAt                  time.Time `json:"due_at"`
	Notes                  string    `json:"notes,omitzero"`
}

// CreateReminderResponse returns the new reminder ID.
type CreateReminderResponse struct {
	ReminderID string `json:"reminder_id"`
}

// CancelReminderRequest is the body for POST /api/v1/crm/reminders/{reminderId}/cancel.
// `reason` is REQUIRED (audit doctrine).
type CancelReminderRequest struct {
	Reason string `json:"reason"`
}

// ----- Error code constants -------------------------------------------------

const (
	errCodeInvalidBody         = "invalid_body"
	errCodeInvalidLeadID       = "invalid_lead_id"
	errCodeInvalidMembershipID = "invalid_membership_id"
	errCodeInvalidStage        = "invalid_stage"
	errCodeInvalidTemperature  = "invalid_temperature"
	errCodeInvalidOutcome      = "invalid_outcome"
	errCodeInvalidCursor       = "invalid_cursor"
	errCodeLeadNotFound        = "lead_not_found"
	errCodeLeadTerminal        = "lead_terminal"
	errCodeReasonRequired      = "reason_required"
	errCodeForbidden           = "forbidden"
	errCodeUnauthenticated     = "unauthenticated"
	errCodeInternalError       = "internal_error"
	errCodeInvalidReminderID   = "invalid_reminder_id"
	errCodeInvalidDueAt        = "invalid_due_at"
	errCodeReminderNotFound    = "reminder_not_found"
	errCodeReminderTerminal    = "reminder_terminal"
)

// errorResponse is the wire shape for non-success responses. RFC 9457
// Problem Details fields kept short (the identity-side shape has a
// fuller form; CRM mirrors the minimum subset for slice 1).
type errorResponse struct {
	Type    string              `json:"type,omitzero"`
	Title   string              `json:"title,omitzero"`
	Status  int                 `json:"status,omitzero"`
	Detail  string              `json:"detail,omitzero"`
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}
