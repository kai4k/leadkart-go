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
	errCodeCreditNoteInvalid  = "credit_note_invalid"
	// errCodeTokenAlreadyPaid: the order's token payment was already recorded (409).
	errCodeTokenAlreadyPaid = "token_payment_already_recorded"
	// errCodeTokenRoute: POST /payments cannot take kind=token; use /token-payment (422).
	errCodeTokenRoute = "token_payment_route_required"
	// errCodeCreditNoteExceedsInvoice: the notes would add up to more than the invoice (422).
	errCodeCreditNoteExceedsInvoice = "credit_note_exceeds_invoice" //nolint:gosec // G101: error code, not a credential
	errCodeUnauthenticated          = "unauthenticated"
	errCodeInternalError            = "internal_error"
)
