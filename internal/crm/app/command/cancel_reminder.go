package command

import (
	"context"
	"errors"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// CancelReminderCommand carries a cancellation request. Reason is
// REQUIRED (audit doctrine).
//
// TenantID is the caller's tenant scope (TDL canon per ADR 0062).
type CancelReminderCommand struct {
	TenantID                tenant.ID
	ReminderID              reminder.ID
	CancelledByMembershipID string
	Reason                  string
}

// CancelReminderHandler runs the cancel flow.
type CancelReminderHandler struct {
	reminders reminder.Repository
	now       func() time.Time
}

// NewCancelReminderHandler wires the handler.
func NewCancelReminderHandler(reminders reminder.Repository, now func() time.Time) CancelReminderHandler {
	if reminders == nil {
		panic("command: NewCancelReminderHandler reminders repository required")
	}
	if now == nil {
		now = time.Now
	}
	return CancelReminderHandler{reminders: reminders, now: now}
}

// Handle cancels the reminder. Returns [ErrReminderNotFound] /
// [ErrReminderTerminal] / [reminder.ErrInvalid] on the respective
// failures.
func (h CancelReminderHandler) Handle(ctx context.Context, cmd CancelReminderCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("crm cancel_reminder: tenant id required")
	}
	if cmd.ReminderID.IsZero() {
		return errors.New("crm cancel_reminder: reminder id required")
	}
	if cmd.CancelledByMembershipID == "" {
		return errors.New("crm cancel_reminder: cancelled-by membership id required")
	}
	if cmd.Reason == "" {
		return errors.New("crm cancel_reminder: reason required")
	}
	now := h.now()
	err := h.reminders.UpdateByID(ctx, cmd.TenantID, cmd.ReminderID, func(r *reminder.Reminder) (bool, error) {
		if err := r.Cancel(cmd.CancelledByMembershipID, cmd.Reason, now); err != nil {
			return false, err
		}
		return true, nil
	})
	return mapReminderError(err)
}
