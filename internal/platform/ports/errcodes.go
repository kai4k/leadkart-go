package ports

// Error-code wire constants (coding-standards.md "no magic strings"),
// snake_case matching the identity module.
const (
	ErrCodeInvalidBody               = "invalid_body"
	ErrCodeInvalidLeadForm           = "invalid_lead_form"
	ErrCodeInvalidCallOutcome        = "invalid_call_outcome"
	ErrCodeInvalidPurchaseAmount     = "invalid_purchase_amount"
	ErrCodeInvalidTenantID           = "invalid_tenant_id"
	ErrCodeInvalidContactID          = "invalid_contact_id"
	ErrCodeInvalidLeadID             = "invalid_lead_id"
	ErrCodeInvalidPageSize           = "invalid_page_size"
	ErrCodeInvalidCursor             = "invalid_cursor"
	ErrCodeContactNotFound           = "contact_not_found"
	ErrCodeContactAlreadyTerminal    = "contact_already_terminal"
	ErrCodeLeadNotFound              = "lead_not_found"
	ErrCodeLeadSoldOut               = "lead_sold_out"
	ErrCodeLeadAlreadyPurchased      = "lead_already_purchased"
	ErrCodeInsufficientCredits       = "insufficient_credits"
	ErrCodeCreditConflict            = "credit_conflict"
	ErrCodeInternalError             = "internal_error"
	ErrCodeMembershipContextRequired = "membership_context_required"
)
