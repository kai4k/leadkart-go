// Package ports holds the Platform module's inbound concrete adapters (TDL
// canon): HTTP handlers that translate external requests into Application
// command/query calls.
package ports

import "time"

// ----- UnverifiedContact ----------------------------------------------------

// CreateUnverifiedContactRequest is the POST /unverified-contacts wire shape.
// All BRD §5 lead-form fields (locked); snake_case JSON per Stripe/Auth0 parity.
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

// LogVerificationCallRequest is the POST .../{id}/calls wire shape.
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

// UnverifiedContactDto is the GET /unverified-contacts wire shape.
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

// ListUnverifiedContactsResponse is the cursor-paginated envelope
// (mirrors pagination.Page[T]).
type ListUnverifiedContactsResponse struct {
	Items      []UnverifiedContactDto `json:"items"`
	HasMore    bool                   `json:"has_more"`
	NextCursor string                 `json:"next_cursor,omitzero"`
}

// ----- Marketplace ----------------------------------------------------------

// MarketplaceLeadDto is the GET /marketplace/leads wire shape. Excludes
// email/GST/PAN — those are revealed only post-purchase (prospect privacy).
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

// PurchaseLeadRequest carries the agreed price. Credit charge is fixed at
// 1 per lead (BRD §4.2); AmountPaisa is the forensic price field.
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

// ErrorResponse is the platform-module error envelope. Mirror of
// identity.ports.ErrorResponse (RFC 9457 Problem Details + legacy fields).
type ErrorResponse struct {
	Type    string              `json:"type,omitzero"`
	Title   string              `json:"title,omitzero"`
	Status  int                 `json:"status,omitzero"`
	Detail  string              `json:"detail,omitzero"`
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}
