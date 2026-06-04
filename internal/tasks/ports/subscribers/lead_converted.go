package subscribers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/app/command"
)

// HandlerLeadConverted is the CI-stable handler name for the
// lead-converted subscriber.
const HandlerLeadConverted = "tasks.subscribers.LeadConverted"

// arch-test:idempotency-via-natural-key-precheck — dedup via
// uq_tasks_source_open on (crm_lead, lead_id) source pair.

// FollowUpWindow is the elapsed time between a lead conversion + the
// auto-scheduled follow-up reminder per BRD §6.8 mature-account
// reorder canon. Fixed at 90 days for v0.2; future product UX may
// allow tenant-level overrides.
const FollowUpWindow = 90 * 24 * time.Hour

// LeadConvertedSubscriber is the Tasks-side subscriber that turns
// `crm.lead_converted.v1` envelopes into a 90-day follow-up
// reminder for the converting salesperson per BRD §6.8.
type LeadConvertedSubscriber struct {
	create command.AutoCreateFollowUpHandler
	log    *slog.Logger
	now    func() time.Time
}

// NewLeadConvertedSubscriber wires the subscriber. `now` is the
// injected clock (Pure Domain canon — ADR 0047); nil → time.Now.
func NewLeadConvertedSubscriber(create command.AutoCreateFollowUpHandler, log *slog.Logger, now func() time.Time) *LeadConvertedSubscriber {
	if log == nil {
		panic("subscribers: NewLeadConvertedSubscriber log required")
	}
	if now == nil {
		now = time.Now
	}
	return &LeadConvertedSubscriber{create: create, log: log, now: now}
}

// Handle is the typed cqrs handler for `crm.lead_converted.v1`. Topic routing +
// payload decode are owned by the EventProcessor (ADR 0067); this schedules a
// 90-day follow-up for the converting salesperson. Returns nil for duplicate
// fires (the source-uniqueness partial index did its job).
func (h *LeadConvertedSubscriber) Handle(ctx context.Context, evt *integrationevents.CrmLeadConvertedV1) error {
	leadID := evt.LeadID.String()
	due := h.now().UTC().Add(FollowUpWindow)
	out, err := h.create.Handle(ctx, command.AutoCreateFollowUpCommand{
		TenantID:                tenant.ID(evt.TenantIDClaim.String()),
		LeadID:                  leadID,
		ConvertedByMembershipID: evt.ConvertedByMembershipID.String(),
		DueAt:                   due,
	})
	if err != nil {
		// retry — source-uniqueness partial index makes the replay safe.
		return fmt.Errorf("tasks subscribers: auto-create follow-up: %w", err)
	}
	if out.AlreadyExisted {
		h.log.InfoContext(ctx, "tasks: follow-up duplicate (idempotency hit)",
			"lead_id", leadID, "work_item_id", out.WorkItemID.String())
		return nil
	}
	h.log.InfoContext(ctx, "tasks: follow-up auto-created",
		"lead_id", leadID, "work_item_id", out.WorkItemID.String(),
		"tenant_id", evt.TenantIDClaim.String(), "due_at", due.Format(time.RFC3339))
	return nil
}
