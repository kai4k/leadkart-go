// Package ports holds the companysettings inbound adapters: the HTTP routes and
// the event subscribers.
package ports

import (
	"encoding/json"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

// ----- Settings --------------------------------------------------------------

// SettingsDto is the company commerce policy: the policy, the version the next
// save must name in expected_version, and the option metadata (every patchable
// setting with its options, bounds, default and the permission that changes it).
type SettingsDto struct {
	TenantID              string                       `json:"tenant_id"`
	Version               int64                        `json:"version"`
	Policy                commercepolicy.Policy        `json:"policy"`
	Options               []commercepolicy.SettingInfo `json:"options"`
	UpdatedAt             *time.Time                   `json:"updated_at,omitzero"`
	UpdatedByMembershipID string                       `json:"updated_by_membership_id,omitzero"`
}

// PatchSettingsRequest changes settings. changes mirrors the policy shape: group,
// then setting name (`{"expiry": {"stop_months_before": 2}}`). Only the settings
// sent change. expected_version is the version the form was loaded at; reason is
// required when a tax, document or compliance setting changes.
type PatchSettingsRequest struct {
	ExpectedVersion *int64                                `json:"expected_version"`
	Reason          string                                `json:"reason"`
	Changes         map[string]map[string]json.RawMessage `json:"changes"`
}

// HistoryEntryDto is one stored version of the policy.
type HistoryEntryDto struct {
	Version                int64                 `json:"version"`
	Policy                 commercepolicy.Policy `json:"policy"`
	ChangedKeys            []string              `json:"changed_keys"`
	Reason                 string                `json:"reason"`
	ActorMembershipID      string                `json:"actor_membership_id,omitzero"`
	ImpersonatorOperatorID string                `json:"impersonator_operator_id,omitzero"`
	ChangedAt              time.Time             `json:"changed_at"`
}

// ListHistoryResponse is one page of the history, newest version first.
type ListHistoryResponse struct {
	Items      []HistoryEntryDto `json:"items"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor,omitzero"`
}

// ----- GST rates -------------------------------------------------------------

// GSTRateDto is one entry of the company's GST rate list.
type GSTRateDto struct {
	ID          string `json:"id"`
	RateBps     int    `json:"rate_bps"`
	Label       string `json:"label"`
	ConfirmedBy string `json:"confirmed_by"`
	ConfirmedOn string `json:"confirmed_on"`
	Active      bool   `json:"active"`
}

// GSTRateChangedDto is a rate after a change, with the policy version the change
// moved to.
type GSTRateChangedDto struct {
	GSTRateDto
	PolicyVersion int64 `json:"policy_version"`
}

// ListGSTRatesResponse is the company's rate list (empty until the company adds
// one) and the policy version it was read at.
type ListGSTRatesResponse struct {
	PolicyVersion int64        `json:"policy_version"`
	Items         []GSTRateDto `json:"items"`
}

// CreateGSTRateRequest adds a rate. confirmed_by names the person who confirmed
// the rate (the company, not LeadKart, vouches for it); confirmed_on is a date
// (YYYY-MM-DD); reason is required.
type CreateGSTRateRequest struct {
	RateBps     *int   `json:"rate_bps"`
	Label       string `json:"label"`
	ConfirmedBy string `json:"confirmed_by"`
	ConfirmedOn string `json:"confirmed_on"`
	Reason      string `json:"reason"`
}

// PatchGSTRateRequest edits or deactivates a rate. A field not sent is left
// alone; the rate itself never changes (add a new rate). reason is required.
type PatchGSTRateRequest struct {
	Label       *string `json:"label"`
	ConfirmedBy *string `json:"confirmed_by"`
	ConfirmedOn *string `json:"confirmed_on"`
	Active      *bool   `json:"active"`
	Reason      string  `json:"reason"`
}

// ----- Error envelope --------------------------------------------------------

// ErrorResponse mirrors the other modules' RFC 9457 Problem Details shape with
// the LeadKart legacy fields preserved.
type ErrorResponse struct {
	// RFC 9457 fields
	Type   string `json:"type,omitzero"`
	Title  string `json:"title,omitzero"`
	Status int    `json:"status,omitzero"`
	Detail string `json:"detail,omitzero"`
	// LeadKart legacy fields
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
	// CurrentVersion is the stored version, on a settings_version_conflict.
	CurrentVersion *int64 `json:"current_version,omitzero"`
}

// Error codes: the snake_case enum surfaced in the legacy `error` field.
const (
	ErrCodeInvalidBody             = "invalid_body"
	ErrCodeInvalidQuery            = "invalid_query"
	ErrCodeInvalidID               = "invalid_id"
	ErrCodeValidationFailed        = "validation_failed"
	ErrCodeNotFound                = "not_found"
	ErrCodeUnauthenticated         = "unauthenticated"
	ErrCodeForbidden               = "forbidden"
	ErrCodeSettingsVersionConflict = "settings_version_conflict"
	ErrCodeInternalError           = "internal_error"
)
