package ports

// Closed catalogue of error codes the Orders HTTP layer emits. Wire-stable.
const (
	errCodeInvalidBody        = "invalid_body"
	errCodeInvalidQuotationID = "invalid_quotation_id"
	errCodeInvalidOrderID     = "invalid_order_id"
	errCodeQuotationNotFound  = "quotation_not_found"
	errCodeOrderNotFound      = "order_not_found"
	errCodeInvoiceNotFound    = "invoice_not_found"
	errCodeInvalidTransition  = "invalid_state_transition"
	errCodeInvoiceConflict    = "invoice_already_exists"
	errCodePaymentConflict    = "payment_duplicate_reference"
	errCodeReasonRequired     = "reason_required"
	errCodeValidation         = "validation_failed"
	errCodeForbidden          = "forbidden"
	errCodeUnauthenticated    = "unauthenticated"
	errCodeInternalError      = "internal_error"
)
