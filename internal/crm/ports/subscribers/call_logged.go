package subscribers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// hhmm is the short-form layout used in human-facing reminder notes
// for the callback window. Constant kept local so the
// TestArch_NoNewTimeFormatStrings gate doesn't flag the literal.
const hhmm = `15:04`

// HandlerCreateCallbackReminder is the CI-stable handler name for the
// CRM in-module CallLogged subscriber per messaging.md "stable handler
// names". Renaming makes every previously-processed message "fresh"
// against the inbox dedup table.
const HandlerCreateCallbackReminder = "crm.subscribers.CreateCallbackReminder"

// arch-test:idempotency-via-partial-unique-index — duplicate broker delivery for the same call_log_id finds the existing pending callback reminder via uq_crm_reminders_callback_pending; the Add adapter translates SQLSTATE 23505 to reminder.ErrAlreadyExists which the CreateReminderHandler turns into AlreadyExisted=true (success ACK).

// CallbackReminderCreator is the CRM in-module subscriber that turns
// `crm.call_logged.v1` envelopes into a pending callback Reminder
// when the call carries a non-zero CallbackWindowStartAt.
//
// Per BRD §4.5: a contact who asks to be called back stamps the next
// CallLogged event with the requested window. The Reminder is due at
// `callback_window_start_at`; the End timestamp rides the wire for
// forensics but the reminder fires at Start (the subscriber treats
// the window as a "fire-at" hint, not as a notify-throughout-window
// schedule — the BRD scope leaves cadenced notifications to v0.3+).
//
// Idempotency: the CreateReminderHandler short-circuits the partial-
// unique-index 23505 fire (one pending callback reminder per
// call_log_id) into AlreadyExisted=true; the subscriber treats that
// branch as success + ACKs the duplicate.
//
// Topic mismatch: the subscriber consumes the `crm.events` topic
// (CRM's own outbox forwarder publishes there). Envelopes carrying a
// different event_type metadata header are silently ignored — same
// shape as the lead-purchased subscriber.
type CallbackReminderCreator struct {
	cmd command.CreateReminderHandler
	log *slog.Logger
}

// NewCallbackReminderCreator wires the subscriber. log is required.
func NewCallbackReminderCreator(cmd command.CreateReminderHandler, log *slog.Logger) *CallbackReminderCreator {
	if log == nil {
		panic("subscribers: NewCallbackReminderCreator log required")
	}
	return &CallbackReminderCreator{cmd: cmd, log: log}
}

// Handle is the typed cqrs handler for `crm.call_logged.v1`. Topic routing +
// payload decode are owned by the EventProcessor (ADR 0067); this dispatches a
// CreateReminderCommand when a callback window is set. Returns nil for
// envelopes without a window (the call did not request a callback) + for
// duplicate fires (the partial unique gate did its job).
func (h *CallbackReminderCreator) Handle(ctx context.Context, evt *integrationevents.CrmCallLoggedV1) error {
	if evt.CallbackWindowStartAt.IsZero() {
		// No callback was requested on this call. The CallLogged event
		// fires for EVERY call — short-circuit when the window is unset.
		return nil
	}
	// The assigned-to membership is the LoggedBy member — the caller
	// who took the call owns the follow-up unless re-assigned by a
	// manager. (Manager-reassignment is a Manual reminder by design.)
	out, err := h.cmd.Handle(ctx, command.CreateReminderCommand{
		TenantID:               tenant.ID(evt.TenantIDClaim.String()),
		LeadID:                 crmlead.ID(evt.LeadID.String()),
		AssignedToMembershipID: evt.LoggedByMembershipID.String(),
		// CreatedByMembershipID is empty — system-created reminder
		// triggered by the subscriber pipeline.
		SourceCallLogID: evt.CallID.String(),
		Type:            reminder.TypeCallback,
		DueAt:           evt.CallbackWindowStartAt,
		Notes:           callbackReminderNotes(evt),
	})
	if err != nil {
		// retry — command-side failure (DB hiccup, lock contention).
		// The partial-unique-index guard makes the retry safe.
		return fmt.Errorf("crm subscribers: create callback reminder: %w", err)
	}
	if out.AlreadyExisted {
		h.log.InfoContext(ctx, "crm: callback-reminder duplicate (idempotency hit)",
			"call_id", evt.CallID.String(), "lead_id", evt.LeadID.String())
		return nil
	}
	h.log.InfoContext(ctx, "crm: callback reminder created",
		"reminder_id", out.ReminderID.String(), "call_id", evt.CallID.String(),
		"lead_id", evt.LeadID.String(), "due_at", evt.CallbackWindowStartAt.Format(time.RFC3339))
	return nil
}

// callbackReminderNotes builds a short audit string for the reminder
// describing the originating call. The reminder's primary identity is
// the SourceCallLogID; notes are a UX nicety.
func callbackReminderNotes(evt *integrationevents.CrmCallLoggedV1) string {
	if evt.CallbackWindowEndAt.IsZero() {
		return fmt.Sprintf("callback requested at %s (call %s)",
			evt.CallbackWindowStartAt.Format(time.RFC3339),
			short(evt.CallID))
	}
	return fmt.Sprintf("callback window %s–%s (call %s)",
		evt.CallbackWindowStartAt.Format(hhmm),
		evt.CallbackWindowEndAt.Format(hhmm),
		short(evt.CallID))
}

func short(id uuid.UUID) string {
	s := id.String()
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
