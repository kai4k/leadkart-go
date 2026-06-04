package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// ----- AutoCreateFromCallLog ------------------------------------------------

// AutoCreateFromCallLogCommand is invoked by the CRM CallLogged
// subscriber when a logged call records a callback window. Creates a
// callback_reminder task assigned to the call's logger. Idempotent
// via the source-uniqueness partial index.
type AutoCreateFromCallLogCommand struct {
	TenantID             tenant.ID
	CallLogID            string // source.EntityID = call_log uuid
	LoggedByMembershipID string
	CallbackAt           time.Time
	LeadID               string // for the task title; empty allowed
	LeadContactName      string // for the task title; empty allowed
}

// AutoCreateFromCallLogResult lets the subscriber distinguish
// fresh-create from idempotency-hit (replay).
type AutoCreateFromCallLogResult struct {
	WorkItemID     workitem.ID
	AlreadyExisted bool
}

// AutoCreateFromCallLogHandler runs the auto-create flow.
type AutoCreateFromCallLogHandler struct {
	repo          workitem.Repository
	now           func() time.Time
	newWorkItemID func() workitem.ID
}

// NewAutoCreateFromCallLogHandler wires the handler.
func NewAutoCreateFromCallLogHandler(repo workitem.Repository, now func() time.Time, newWorkItemID func() workitem.ID) AutoCreateFromCallLogHandler {
	if repo == nil {
		panic("command: NewAutoCreateFromCallLogHandler repo required")
	}
	if newWorkItemID == nil {
		panic("command: NewAutoCreateFromCallLogHandler newWorkItemID required")
	}
	if now == nil {
		now = time.Now
	}
	return AutoCreateFromCallLogHandler{repo: repo, now: now, newWorkItemID: newWorkItemID}
}

// Handle persists the callback_reminder task. Returns
// AlreadyExisted=true when the partial-unique-index trips (replay).
func (h AutoCreateFromCallLogHandler) Handle(ctx context.Context, cmd AutoCreateFromCallLogCommand) (AutoCreateFromCallLogResult, error) {
	if cmd.TenantID.IsZero() {
		return AutoCreateFromCallLogResult{}, errors.New("tasks auto_create_call: tenant id required")
	}
	if cmd.CallLogID == "" {
		return AutoCreateFromCallLogResult{}, errors.New("tasks auto_create_call: call_log id required")
	}
	if cmd.LoggedByMembershipID == "" {
		return AutoCreateFromCallLogResult{}, errors.New("tasks auto_create_call: logged-by membership id required")
	}
	if cmd.CallbackAt.IsZero() {
		return AutoCreateFromCallLogResult{}, errors.New("tasks auto_create_call: callback_at required")
	}

	title := "Callback reminder"
	if cmd.LeadContactName != "" {
		title = "Callback: " + cmd.LeadContactName
	}
	now := h.now()
	w, err := workitem.NewAutoCreated(workitem.AutoCreateParams{
		ID:                     h.newWorkItemID(),
		TenantID:               cmd.TenantID,
		Type:                   workitem.TypeCallbackReminder,
		Priority:               workitem.PriorityHigh,
		Title:                  title,
		Description:            "",
		AssignedToMembershipID: cmd.LoggedByMembershipID,
		AssignedByMembershipID: cmd.LoggedByMembershipID,
		CreatedByMembershipID:  cmd.LoggedByMembershipID,
		DueAt:                  cmd.CallbackAt,
		Now:                    now,
		Source: workitem.Source{
			Module:     "crm",
			EntityType: "call_log",
			EntityID:   cmd.CallLogID,
		},
	})
	if err != nil {
		return AutoCreateFromCallLogResult{}, fmt.Errorf("tasks auto_create_call: factory: %w", err)
	}

	if err := h.repo.Add(ctx, w); err != nil {
		if errors.Is(err, workitem.ErrAlreadyExistsForSource) {
			// Look up the existing open task so the subscriber can log
			// which row deduped — best-effort, ignore lookup errors.
			existing, lookupErr := h.repo.GetOpenBySource(ctx, cmd.TenantID, "call_log", cmd.CallLogID)
			if lookupErr == nil && existing != nil {
				return AutoCreateFromCallLogResult{WorkItemID: existing.ID(), AlreadyExisted: true}, nil
			}
			return AutoCreateFromCallLogResult{AlreadyExisted: true}, nil
		}
		return AutoCreateFromCallLogResult{}, fmt.Errorf("tasks auto_create_call: persist: %w", err)
	}
	return AutoCreateFromCallLogResult{WorkItemID: w.ID()}, nil
}

// ----- AutoCreateFollowUp --------------------------------------------------

// AutoCreateFollowUpCommand is invoked by the CRM LeadConverted
// subscriber to seed a 90-day follow-up reminder for the converting
// salesperson per BRD §6.8 mature-account reorder reminders.
type AutoCreateFollowUpCommand struct {
	TenantID                tenant.ID
	LeadID                  string // source.EntityID = crm_lead uuid
	ConvertedByMembershipID string
	DueAt                   time.Time // 90 days post-conversion per BRD
	LeadContactName         string
}

// AutoCreateFollowUpResult lets the subscriber distinguish fresh vs
// idempotent paths.
type AutoCreateFollowUpResult struct {
	WorkItemID     workitem.ID
	AlreadyExisted bool
}

// AutoCreateFollowUpHandler runs the follow-up creation flow.
type AutoCreateFollowUpHandler struct {
	repo          workitem.Repository
	now           func() time.Time
	newWorkItemID func() workitem.ID
}

// NewAutoCreateFollowUpHandler wires the handler.
func NewAutoCreateFollowUpHandler(repo workitem.Repository, now func() time.Time, newWorkItemID func() workitem.ID) AutoCreateFollowUpHandler {
	if repo == nil {
		panic("command: NewAutoCreateFollowUpHandler repo required")
	}
	if newWorkItemID == nil {
		panic("command: NewAutoCreateFollowUpHandler newWorkItemID required")
	}
	if now == nil {
		now = time.Now
	}
	return AutoCreateFollowUpHandler{repo: repo, now: now, newWorkItemID: newWorkItemID}
}

// Handle persists the follow-up task. Idempotent via source pair.
func (h AutoCreateFollowUpHandler) Handle(ctx context.Context, cmd AutoCreateFollowUpCommand) (AutoCreateFollowUpResult, error) {
	if cmd.TenantID.IsZero() {
		return AutoCreateFollowUpResult{}, errors.New("tasks auto_create_follow_up: tenant id required")
	}
	if cmd.LeadID == "" {
		return AutoCreateFollowUpResult{}, errors.New("tasks auto_create_follow_up: lead id required")
	}
	if cmd.ConvertedByMembershipID == "" {
		return AutoCreateFollowUpResult{}, errors.New("tasks auto_create_follow_up: converted-by membership id required")
	}
	if cmd.DueAt.IsZero() {
		return AutoCreateFollowUpResult{}, errors.New("tasks auto_create_follow_up: due_at required")
	}

	title := "Follow up with converted lead"
	if cmd.LeadContactName != "" {
		title = "Follow up: " + cmd.LeadContactName
	}
	now := h.now()
	w, err := workitem.NewAutoCreated(workitem.AutoCreateParams{
		ID:                     h.newWorkItemID(),
		TenantID:               cmd.TenantID,
		Type:                   workitem.TypeFollowUp,
		Priority:               workitem.PriorityMedium,
		Title:                  title,
		Description:            "Auto-scheduled 90-day reorder reminder.",
		AssignedToMembershipID: cmd.ConvertedByMembershipID,
		AssignedByMembershipID: cmd.ConvertedByMembershipID,
		CreatedByMembershipID:  cmd.ConvertedByMembershipID,
		DueAt:                  cmd.DueAt,
		Now:                    now,
		Source: workitem.Source{
			Module:     "crm",
			EntityType: "crm_lead",
			EntityID:   cmd.LeadID,
		},
	})
	if err != nil {
		return AutoCreateFollowUpResult{}, fmt.Errorf("tasks auto_create_follow_up: factory: %w", err)
	}
	if err := h.repo.Add(ctx, w); err != nil {
		if errors.Is(err, workitem.ErrAlreadyExistsForSource) {
			existing, lookupErr := h.repo.GetOpenBySource(ctx, cmd.TenantID, "crm_lead", cmd.LeadID)
			if lookupErr == nil && existing != nil {
				return AutoCreateFollowUpResult{WorkItemID: existing.ID(), AlreadyExisted: true}, nil
			}
			return AutoCreateFollowUpResult{AlreadyExisted: true}, nil
		}
		return AutoCreateFollowUpResult{}, fmt.Errorf("tasks auto_create_follow_up: persist: %w", err)
	}
	return AutoCreateFollowUpResult{WorkItemID: w.ID()}, nil
}

// ----- AutoCompleteBySource ------------------------------------------------

// AutoCompleteBySourceCommand resolves the OPEN work item linked to
// the supplied source pair (if any) and Completes it. Used by the
// "call logged → auto-complete the matching CallbackReminder" flow
// per BRD §6.8.
//
// No-op when no open work item exists for the source pair (returns
// nil error with WorkItemID="").
type AutoCompleteBySourceCommand struct {
	TenantID         tenant.ID
	SourceEntityType string
	SourceEntityID   string
	ActorID          string
}

// AutoCompleteBySourceResult signals whether a row was completed.
type AutoCompleteBySourceResult struct {
	WorkItemID workitem.ID
	NoMatch    bool
}

// AutoCompleteBySourceHandler runs the auto-complete flow.
type AutoCompleteBySourceHandler struct {
	repo workitem.Repository
	now  func() time.Time
}

// NewAutoCompleteBySourceHandler wires the handler.
func NewAutoCompleteBySourceHandler(repo workitem.Repository, now func() time.Time) AutoCompleteBySourceHandler {
	if repo == nil {
		panic("command: NewAutoCompleteBySourceHandler repo required")
	}
	if now == nil {
		now = time.Now
	}
	return AutoCompleteBySourceHandler{repo: repo, now: now}
}

// Handle resolves + completes the matching work item; returns
// NoMatch=true if none exists.
func (h AutoCompleteBySourceHandler) Handle(ctx context.Context, cmd AutoCompleteBySourceCommand) (AutoCompleteBySourceResult, error) {
	if cmd.TenantID.IsZero() {
		return AutoCompleteBySourceResult{}, errors.New("tasks auto_complete: tenant id required")
	}
	if cmd.SourceEntityType == "" || cmd.SourceEntityID == "" {
		return AutoCompleteBySourceResult{}, errors.New("tasks auto_complete: source pair required")
	}
	if cmd.ActorID == "" {
		return AutoCompleteBySourceResult{}, errors.New("tasks auto_complete: actor id required")
	}
	w, err := h.repo.GetOpenBySource(ctx, cmd.TenantID, cmd.SourceEntityType, cmd.SourceEntityID)
	if err != nil {
		if errors.Is(err, workitem.ErrNotFound) {
			return AutoCompleteBySourceResult{NoMatch: true}, nil
		}
		return AutoCompleteBySourceResult{}, fmt.Errorf("tasks auto_complete: lookup: %w", err)
	}
	now := h.now()
	err = h.repo.UpdateByID(ctx, cmd.TenantID, w.ID(), func(wi *workitem.WorkItem) (bool, error) {
		old := wi.State()
		if err := wi.Complete(cmd.ActorID, now); err != nil {
			return false, err
		}
		if wi.State() == old {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return AutoCompleteBySourceResult{}, mapErr(err)
	}
	return AutoCompleteBySourceResult{WorkItemID: w.ID()}, nil
}
