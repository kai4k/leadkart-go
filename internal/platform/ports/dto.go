// Package ports holds Platform module inbound concrete implementations
// per TDL canon — HTTP handlers + future event subscribers. Packages
// under here translate external requests into Application command/
// query handler calls.
package ports

import "time"

// ----- UnverifiedContact ----------------------------------------------------

// CreateUnverifiedContactRequest is the wire shape for POST
// /api/v1/platform/unverified-contacts.
//
// All BRD §5 lead form fields (locked). JSON snake_case for tooling
// + Stripe / Auth0 wire-convention parity.
type CreateUnverifiedContactRequest struct {
	ContactName    string   `json:"contact_name"`
	MobileE164     string   `json:"mobile_e164"`
	Email          string   `json:"email,omitzero"`
	Pincode        string   `json:"pincode"`
	City           string   `json:"city"`
	District       string   `json:"district"`
	State          string   `json:"state"`
	Street         string   `json:"street,omitzero"`
	HasDrugLicence bool     `json:"has_drug_licence"`
	HasGst         bool     `json:"has_gst"`
	GstNumber      string   `json:"gst_number,omitzero"`
	HasPan         bool     `json:"has_pan"`
	PanNumber      string   `json:"pan_number,omitzero"`
	BusinessType   string   `json:"business_type"`
	MedicineSystem string   `json:"medicine_system"`
	ProductRanges  []string `json:"product_ranges"`
	DosageForms    []string `json:"dosage_forms"`
	OrderValue     string   `json:"order_value"`
	BuyTimeline    string   `json:"buy_timeline"`
}

// CreateUnverifiedContactResponse — 201 body, ID only.
type CreateUnverifiedContactResponse struct {
	ContactID string `json:"contact_id"`
}

// LogVerificationCallRequest is the wire shape for POST
// .../unverified-contacts/{id}/calls.
type LogVerificationCallRequest struct {
	Outcome               string    `json:"outcome"`
	Notes                 string    `json:"notes,omitzero"`
	CallbackWindowStartAt time.Time `json:"callback_window_start_at,omitzero"`
	CallbackWindowEndAt   time.Time `json:"callback_window_end_at,omitzero"`
}

// LogVerificationCallResponse — 201 body, call ID.
type LogVerificationCallResponse struct {
	CallID string `json:"call_id"`
}

// VerifyUnverifiedContactRequest — empty body (intent comes from URL).
type VerifyUnverifiedContactRequest struct{}

// VerifyUnverifiedContactResponse — 201 body, new PlatformLead ID.
type VerifyUnverifiedContactResponse struct {
	PlatformLeadID string `json:"platform_lead_id"`
}

// RejectUnverifiedContactRequest carries the audit reason.
type RejectUnverifiedContactRequest struct {
	Reason string `json:"reason"`
}

// UnverifiedContactDto is the wire shape returned by
// GET /api/v1/platform/unverified-contacts.
type UnverifiedContactDto struct {
	ID                    string `json:"id"`
	State                 string `json:"state"`
	ContactName           string `json:"contact_name"`
	MobileE164            string `json:"mobile_e164"`
	City                  string `json:"city"`
	StateGeo              string `json:"state_geo"`
	CreatedAt             string `json:"created_at"`
	CreatedByMembershipID string `json:"created_by_membership_id"`
}

// ListUnverifiedContactsResponse is the cursor-paginated wire envelope.
// Mirrors pagination.Page[T] shape from internal/common/pagination/.
type ListUnverifiedContactsResponse struct {
	Items      []UnverifiedContactDto `json:"items"`
	HasMore    bool                   `json:"has_more"`
	NextCursor string                 `json:"next_cursor,omitzero"`
}

// ----- Marketplace ----------------------------------------------------------

// MarketplaceLeadDto is the wire shape returned by GET
// /api/v1/platform/marketplace/leads. Excludes email/GST/PAN —
// available only post-purchase to protect prospect privacy.
type MarketplaceLeadDto struct {
	ID             string    `json:"id"`
	ContactName    string    `json:"contact_name"`
	City           string    `json:"city"`
	District       string    `json:"district"`
	State          string    `json:"state"`
	Pincode        string    `json:"pincode"`
	HasDrugLicence bool      `json:"has_drug_licence"`
	HasGst         bool      `json:"has_gst"`
	GstVerified    bool      `json:"gst_verified"`
	HasPan         bool      `json:"has_pan"`
	BusinessType   string    `json:"business_type"`
	MedicineSystem string    `json:"medicine_system"`
	ProductRanges  []string  `json:"product_ranges"`
	DosageForms    []string  `json:"dosage_forms"`
	OrderValue     string    `json:"order_value"`
	BuyTimeline    string    `json:"buy_timeline"`
	VerifiedAt     time.Time `json:"verified_at"`
}

// BrowseMarketplaceResponse — paginated wire envelope.
type BrowseMarketplaceResponse struct {
	Items      []MarketplaceLeadDto `json:"items"`
	HasMore    bool                 `json:"has_more"`
	NextCursor string               `json:"next_cursor,omitzero"`
}

// PurchaseLeadRequest carries the price the tenant agreed to pay. The
// charge against the lead-credit balance is FIXED at 1 credit per lead
// (BRD §4.2); AmountPaisa is the forensic price field.
type PurchaseLeadRequest struct {
	AmountPaisa int64 `json:"amount_paisa"`
}

// PurchaseLeadResponse — 201 body, purchase ID for CRM correlation.
type PurchaseLeadResponse struct {
	PurchaseID     string `json:"purchase_id"`
	PlatformLeadID string `json:"platform_lead_id"`
}

// ----- LeadCredits ----------------------------------------------------------

// TopupLeadCreditsRequest — operator-driven credit grant.
type TopupLeadCreditsRequest struct {
	TenantID string `json:"tenant_id"`
	Delta    int64  `json:"delta"`
	Reason   string `json:"reason"`
}

// TopupLeadCreditsResponse — 200 body, post-topup balance.
type TopupLeadCreditsResponse struct {
	TenantID   string `json:"tenant_id"`
	NewBalance int64  `json:"new_balance"`
}

// LeadCreditBalanceResponse — 200 body, current balance.
type LeadCreditBalanceResponse struct {
	TenantID string `json:"tenant_id"`
	Balance  int64  `json:"balance"`
}

// ----- Errors --------------------------------------------------------------

// ErrorResponse is the canonical platform-module error envelope.
// Mirror of identity.ports.ErrorResponse (RFC 9457 Problem Details +
// legacy fields).
type ErrorResponse struct {
	Type    string              `json:"type,omitzero"`
	Title   string              `json:"title,omitzero"`
	Status  int                 `json:"status,omitzero"`
	Detail  string              `json:"detail,omitzero"`
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}
