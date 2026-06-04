package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ReminderView is the wire-shaped read model for the pending-reminders
// dashboard — strict CQRS (ADR 0060): the query projects the aggregate to a
// View so the domain type never leaks to the port.
type ReminderView struct {
	ID                       string
	TenantID                 string
	LeadID                   string
	AssignedToMembershipID   string
	CreatedByMembershipID    string
	SourceCallLogID          string
	Type                     string
	State                    string
	DueAt                    time.Time
	Notes                    string
	SentAt                   time.Time
	MarkedSentByMembershipID string
	CancelledAt              time.Time
	CancelledByMembershipID  string
	CancelReason             string
	CreatedAt                time.Time
}

func reminderToView(r *reminder.Reminder) ReminderView {
	return ReminderView{
		ID:                       r.ID().String(),
		TenantID:                 r.TenantID().String(),
		LeadID:                   r.LeadID().String(),
		AssignedToMembershipID:   r.AssignedToMembershipID(),
		CreatedByMembershipID:    r.CreatedByMembershipID(),
		SourceCallLogID:          r.SourceCallLogID(),
		Type:                     r.Type().String(),
		State:                    r.State().String(),
		DueAt:                    r.DueAt(),
		Notes:                    r.Notes(),
		SentAt:                   r.SentAt(),
		MarkedSentByMembershipID: r.MarkedSentByMembershipID(),
		CancelledAt:              r.CancelledAt(),
		CancelledByMembershipID:  r.CancelledByMembershipID(),
		CancelReason:             r.CancelReason(),
		CreatedAt:                r.CreatedAt(),
	}
}

// ListPendingRemindersQuery carries the cursor-pagination + filter
// inputs for the dashboard "today / upcoming / overdue" view per BRD
// §4.6. PageSize is clamped per ADR 0038.
//
// SelfFilter is the per-handler "only my reminders" enforcement — HTTP
// layer populates it from the JWT membership claim when the caller
// LACKS `crm.leads.read_all` (mirrors the leads list rule). When set +
// non-zero, overrides Filter.AssigneeMembershipID.
type ListPendingRemindersQuery struct {
	TenantID   tenant.ID
	Cursor     pagination.Cursor
	PageSize   int
	Filter     reminder.PendingFilter
	SelfFilter string
}

// ListPendingRemindersHandler runs the dashboard list.
type ListPendingRemindersHandler struct {
	reminders reminder.Repository
}

// NewListPendingRemindersHandler wires the handler.
func NewListPendingRemindersHandler(reminders reminder.Repository) ListPendingRemindersHandler {
	if reminders == nil {
		panic("query: NewListPendingRemindersHandler reminders repository required")
	}
	return ListPendingRemindersHandler{reminders: reminders}
}

// Handle returns the paginated pending-reminder page projected to read-model
// Views (strict CQRS — the aggregate must not reach the port).
func (h ListPendingRemindersHandler) Handle(ctx context.Context, q ListPendingRemindersQuery) (pagination.Page[ReminderView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[ReminderView]{}, errors.New("crm list_pending_reminders: tenant id required")
	}
	filter := q.Filter
	if q.SelfFilter != "" {
		filter.AssigneeMembershipID = q.SelfFilter
	}
	page, err := h.reminders.ListPagePending(ctx, q.TenantID, filter, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[ReminderView]{}, fmt.Errorf("crm list_pending_reminders: %w", err)
	}
	views := make([]ReminderView, 0, len(page.Items))
	for _, r := range page.Items {
		views = append(views, reminderToView(r))
	}
	return pagination.Page[ReminderView]{Items: views, HasMore: page.HasMore, NextCursor: page.NextCursor}, nil
}
