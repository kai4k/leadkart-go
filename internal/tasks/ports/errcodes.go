package ports

// Closed catalogue of error codes the Tasks HTTP layer emits. Wire-stable.
const (
	errCodeInvalidBody         = "invalid_body"
	errCodeInvalidWorkItemID   = "invalid_work_item_id"
	errCodeInvalidMembershipID = "invalid_membership_id"
	errCodeInvalidType         = "invalid_type"
	errCodeInvalidPriority     = "invalid_priority"
	errCodeInvalidState        = "invalid_state"
	errCodeInvalidCursor       = "invalid_cursor"
	errCodeInvalidDueAt        = "invalid_due_at"
	errCodeWorkItemNotFound    = "work_item_not_found"
	errCodeWorkItemTerminal    = "work_item_terminal"
	errCodeReasonRequired      = "reason_required"
	errCodeAssigneeInactive    = "assignee_inactive"
	errCodeReassignForbidden   = "reassign_forbidden"
	errCodeForbidden           = "forbidden"
	errCodeUnauthenticated     = "unauthenticated"
	errCodeInternalError       = "internal_error"
)
