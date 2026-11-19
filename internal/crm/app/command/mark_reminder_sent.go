package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// MarkReminderSentCommand carries a mark-sent request.
//
// TenantID is the caller's tenant scope (TDL canon per ADR 0062 — flows
// through as an explicit value, not via ctx-tenancy).
type MarkReminderSentCommand struct {
	TenantID             tenant.ID
	ReminderID           reminder.ID
	MarkedByMembershipID string
	// OnlyAssignedTo, when set, limits the command to a reminder assigned to
	// this membership: any other reminder does not exist for the caller
	// (ErrReminderNotFound). Empty means unrestricted.
	OnlyAssignedTo string
}

// MarkReminderSentHandler runs the mark-sent flow.
type MarkReminderSentHandler struct {
	reminders reminder.Repository
	now       func() time.Time
}

// NewMarkReminderSentHandler wires the handler. `now` is the injected
// wall-clock (Pure Domain canon — ADR 0047); nil → time.Now.
func NewMarkReminderSentHandler(reminders reminder.Repository, now func() time.Time) MarkReminderSentHandler {
	if reminders == nil {
		panic("command: NewMarkReminderSentHandler reminders repository required")
	}
	if now == nil {
		now = time.Now
	}
	return MarkReminderSentHandler{reminders: reminders, now: now}
}

// Handle marks the reminder sent. Returns [ErrReminderNotFound] when
// the row doesn't exist in the caller's tenant; [ErrReminderTerminal]
// when the reminder is already in a terminal state.
func (h MarkReminderSentHandler) Handle(ctx context.Context, cmd MarkReminderSentCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("crm mark_reminder_sent: tenant id required")
	}
	if cmd.ReminderID.IsZero() {
		return errors.New("crm mark_reminder_sent: reminder id required")
	}
	if cmd.MarkedByMembershipID == "" {
		return errors.New("crm mark_reminder_sent: marked-by membership id required")
	}
	now := h.now()
	err := h.reminders.UpdateByID(ctx, cmd.TenantID, cmd.ReminderID, func(r *reminder.Reminder) (bool, error) {
		if !reminderInScope(r, cmd.OnlyAssignedTo) {
			return false, reminder.ErrNotFound
		}
		if err := r.MarkSent(cmd.MarkedByMembershipID, now); err != nil {
			return false, err
		}
		return true, nil
	})
	return mapReminderError(err)
}

// reminderInScope mirrors leadInScope for a reminder: the caller's
// OnlyAssignedTo must be the reminder's assignee. Empty is unrestricted.
func reminderInScope(r *reminder.Reminder, onlyAssignedTo string) bool {
	return onlyAssignedTo == "" || r.AssignedToMembershipID() == onlyAssignedTo
}

// mapReminderError collapses the common reminder errors to the app-layer
// sentinels HTTP handlers branch on.
func mapReminderError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, reminder.ErrNotFound):
		return ErrReminderNotFound
	case errors.Is(err, reminder.ErrConflict):
		return ErrReminderTerminal
	default:
		return fmt.Errorf("crm reminder: %w", err)
	}
}
