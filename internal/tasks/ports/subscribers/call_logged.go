package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/app/command"
)

// HandlerCallLogged is the CI-stable handler name for the call-logged
// subscriber. Changing it makes every previously-processed message
// "fresh" against the inbox dedup table — DO NOT rename.
const HandlerCallLogged = "tasks.subscribers.CallLogged"

// arch-test:idempotency-via-natural-key-precheck — dedup happens one
// call-frame down: command.AutoCreateFromCallLogHandler.Handle hits
// the partial-unique-index uq_tasks_source_open and short-circuits
// with AlreadyExisted=true on replay. The handler returns nil on that
// branch so Watermill ACKs the duplicate.

// CallLoggedSubscriber is the Tasks-side subscriber that turns
// `crm.call_logged.v1` envelopes into callback_reminder work items —
// when the call payload carries a callback_window_start_at, that
// timestamp becomes the new task's due_at; the call's logger becomes
// the assignee.
//
// ALSO runs the auto-complete-by-source flow per BRD §6.8 — if an
// open work item exists for the SAME call_log id, the new call event
// closes it. v0.2 calls CRM never set callback_window_start_at on
// the same row twice, but the symmetry stays so future call-log
// updates close stale reminders cleanly.
type CallLoggedSubscriber struct {
	create       command.AutoCreateFromCallLogHandler
	autoComplete command.AutoCompleteBySourceHandler
	log          *slog.Logger
}

// NewCallLoggedSubscriber wires the subscriber.
func NewCallLoggedSubscriber(
	create command.AutoCreateFromCallLogHandler,
	autoComplete command.AutoCompleteBySourceHandler,
	log *slog.Logger,
) *CallLoggedSubscriber {
	if log == nil {
		panic("subscribers: NewCallLoggedSubscriber log required")
	}
	return &CallLoggedSubscriber{create: create, autoComplete: autoComplete, log: log}
}

// Handle is the typed cqrs handler for `crm.call_logged.v1`. Topic routing +
// payload decode are owned by the EventProcessor (ADR 0067); this auto-completes
// any open work item bound to the call_log id, then mints a callback_reminder
// when the call recorded a callback window. Returns nil for calls without a
// window + for duplicate fires (the source-uniqueness partial index did its job).
func (h *CallLoggedSubscriber) Handle(ctx context.Context, evt *integrationevents.CrmCallLoggedV1) error {
	tenantID := tenant.ID(evt.TenantIDClaim.String())
	callID := evt.CallID.String()

	// Auto-complete any open work item whose source pair matches this
	// call_log id. No-op when none exists.
	if _, err := h.autoComplete.Handle(ctx, command.AutoCompleteBySourceCommand{
		TenantID:         tenantID,
		SourceEntityType: "call_log",
		SourceEntityID:   callID,
		ActorID:          evt.LoggedByMembershipID.String(),
	}); err != nil {
		// retry — Complete is idempotent on self; replay safe.
		return fmt.Errorf("tasks subscribers: auto-complete: %w", err)
	}

	// Skip auto-create unless the call recorded a callback window.
	if evt.CallbackWindowStartAt.IsZero() {
		return nil
	}
	out, err := h.create.Handle(ctx, command.AutoCreateFromCallLogCommand{
		TenantID:             tenantID,
		CallLogID:            callID,
		LoggedByMembershipID: evt.LoggedByMembershipID.String(),
		CallbackAt:           evt.CallbackWindowStartAt,
	})
	if err != nil {
		// retry — source-uniqueness partial index makes the replay safe.
		return fmt.Errorf("tasks subscribers: auto-create callback: %w", err)
	}
	if out.AlreadyExisted {
		h.log.InfoContext(ctx, "tasks: callback reminder duplicate (idempotency hit)",
			"call_id", callID, "work_item_id", out.WorkItemID.String())
		return nil
	}
	h.log.InfoContext(ctx, "tasks: callback reminder auto-created",
		"call_id", callID, "work_item_id", out.WorkItemID.String(),
		"tenant_id", tenantID.String())
	return nil
}
