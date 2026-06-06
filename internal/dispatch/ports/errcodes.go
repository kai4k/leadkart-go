package ports

// Closed catalogue of error codes the Dispatch HTTP layer emits. Wire-stable.
const (
	errCodeInvalidBody             = "invalid_body"
	errCodeInvalidConsignmentID    = "invalid_consignment_note_id"
	errCodeInvalidOrderID          = "invalid_order_id"
	errCodeOrderIDRequired         = "order_id_required"
	errCodeDocketRequired          = "docket_number_required"
	errCodeReasonRequired          = "reason_required"
	errCodeConsignmentNoteNotFound = "consignment_note_not_found"
	errCodeConsignmentNoteConflict = "consignment_note_conflict"
	errCodeInvalidStatusTransition = "invalid_status_transition"
	errCodeForbidden               = "forbidden"
	errCodeUnauthenticated         = "unauthenticated"
	errCodeInternalError           = "internal_error"
)
