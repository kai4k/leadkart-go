package ports

// Error-code wire constants per `coding-standards.md` "No magic
// strings — production AND tests". Match the identity module's
// conventions: snake_case, semantic prefix per concern.
const (
	ErrCodeInvalidBody                 = "invalid_body"
	ErrCodeInvalidLeadForm             = "invalid_lead_form"
	ErrCodeInvalidCallOutcome          = "invalid_call_outcome"
	ErrCodeInvalidPurchaseAmount       = "invalid_purchase_amount"
	ErrCodeInvalidTenantID             = "invalid_tenant_id"
	ErrCodeInvalidContactID            = "invalid_contact_id"
	ErrCodeInvalidLeadID               = "invalid_lead_id"
	ErrCodeInvalidPageSize             = "invalid_page_size"
	ErrCodeInvalidCursor               = "invalid_cursor"
	ErrCodeContactNotFound             = "contact_not_found"
	ErrCodeContactAlreadyTerminal      = "contact_already_terminal"
	ErrCodeLeadNotFound                = "lead_not_found"
	ErrCodeLeadAlreadySold             = "lead_already_sold"
	ErrCodeInsufficientCredits         = "insufficient_credits"
	ErrCodeCreditConflict              = "credit_conflict"
	ErrCodeInternalError               = "internal_error"
	ErrCodeMembershipContextRequired   = "membership_context_required"
)
