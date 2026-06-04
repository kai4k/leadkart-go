package command

import (
	"context"
	"errors"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// ----- StartWorkItem -------------------------------------------------------

// StartWorkItemCommand carries a start-task request.
type StartWorkItemCommand struct {
	TenantID   tenant.ID
	WorkItemID workitem.ID
	ActorID    string // the membership flipping the task to in_progress
}

// StartWorkItemHandler runs the start-task flow.
type StartWorkItemHandler struct {
	repo workitem.Repository
	now  func() time.Time
}

// NewStartWorkItemHandler wires the handler.
func NewStartWorkItemHandler(repo workitem.Repository, now func() time.Time) StartWorkItemHandler {
	if repo == nil {
		panic("command: NewStartWorkItemHandler repo required")
	}
	if now == nil {
		now = time.Now
	}
	return StartWorkItemHandler{repo: repo, now: now}
}

// Handle flips the task to in_progress.
func (h StartWorkItemHandler) Handle(ctx context.Context, cmd StartWorkItemCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("tasks start: tenant id required")
	}
	if cmd.WorkItemID.IsZero() {
		return errors.New("tasks start: work item id required")
	}
	if cmd.ActorID == "" {
		return errors.New("tasks start: actor id required")
	}
	now := h.now()
	err := h.repo.UpdateByID(ctx, cmd.TenantID, cmd.WorkItemID, func(w *workitem.WorkItem) (bool, error) {
		old := w.State()
		if err := w.Start(cmd.ActorID, now); err != nil {
			return false, err
		}
		if w.State() == old {
			return false, nil
		}
		return true, nil
	})
	return mapErr(err)
}

// ----- CompleteWorkItem ----------------------------------------------------

// CompleteWorkItemCommand carries a complete-task request.
type CompleteWorkItemCommand struct {
	TenantID   tenant.ID
	WorkItemID workitem.ID
	ActorID    string
}

// CompleteWorkItemHandler runs the complete-task flow.
type CompleteWorkItemHandler struct {
	repo workitem.Repository
	now  func() time.Time
}

// NewCompleteWorkItemHandler wires the handler.
func NewCompleteWorkItemHandler(repo workitem.Repository, now func() time.Time) CompleteWorkItemHandler {
	if repo == nil {
		panic("command: NewCompleteWorkItemHandler repo required")
	}
	if now == nil {
		now = time.Now
	}
	return CompleteWorkItemHandler{repo: repo, now: now}
}

// Handle terminally completes the task.
func (h CompleteWorkItemHandler) Handle(ctx context.Context, cmd CompleteWorkItemCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("tasks complete: tenant id required")
	}
	if cmd.WorkItemID.IsZero() {
		return errors.New("tasks complete: work item id required")
	}
	if cmd.ActorID == "" {
		return errors.New("tasks complete: actor id required")
	}
	now := h.now()
	err := h.repo.UpdateByID(ctx, cmd.TenantID, cmd.WorkItemID, func(w *workitem.WorkItem) (bool, error) {
		old := w.State()
		if err := w.Complete(cmd.ActorID, now); err != nil {
			return false, err
		}
		if w.State() == old {
			return false, nil
		}
		return true, nil
	})
	return mapErr(err)
}

// ----- CancelWorkItem ------------------------------------------------------

// CancelWorkItemCommand carries a cancel-task request.
type CancelWorkItemCommand struct {
	TenantID   tenant.ID
	WorkItemID workitem.ID
	ActorID    string
	Reason     string
}

// CancelWorkItemHandler runs the cancel-task flow.
type CancelWorkItemHandler struct {
	repo workitem.Repository
	now  func() time.Time
}

// NewCancelWorkItemHandler wires the handler.
func NewCancelWorkItemHandler(repo workitem.Repository, now func() time.Time) CancelWorkItemHandler {
	if repo == nil {
		panic("command: NewCancelWorkItemHandler repo required")
	}
	if now == nil {
		now = time.Now
	}
	return CancelWorkItemHandler{repo: repo, now: now}
}

// Handle terminally cancels the task.
func (h CancelWorkItemHandler) Handle(ctx context.Context, cmd CancelWorkItemCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("tasks cancel: tenant id required")
	}
	if cmd.WorkItemID.IsZero() {
		return errors.New("tasks cancel: work item id required")
	}
	if cmd.ActorID == "" {
		return errors.New("tasks cancel: actor id required")
	}
	if cmd.Reason == "" {
		return errors.New("tasks cancel: reason required")
	}
	now := h.now()
	err := h.repo.UpdateByID(ctx, cmd.TenantID, cmd.WorkItemID, func(w *workitem.WorkItem) (bool, error) {
		old := w.State()
		if err := w.Cancel(cmd.ActorID, cmd.Reason, now); err != nil {
			return false, err
		}
		if w.State() == old {
			return false, nil
		}
		return true, nil
	})
	return mapErr(err)
}

// ----- MarkOverdue ---------------------------------------------------------

// MarkOverdueCommand carries an overdue-flag request from the
// periodic OverdueScan job.
type MarkOverdueCommand struct {
	TenantID   tenant.ID
	WorkItemID workitem.ID
}

// MarkOverdueHandler runs the overdue-flag flow.
type MarkOverdueHandler struct {
	repo workitem.Repository
	now  func() time.Time
}

// NewMarkOverdueHandler wires the handler.
func NewMarkOverdueHandler(repo workitem.Repository, now func() time.Time) MarkOverdueHandler {
	if repo == nil {
		panic("command: NewMarkOverdueHandler repo required")
	}
	if now == nil {
		now = time.Now
	}
	return MarkOverdueHandler{repo: repo, now: now}
}

// Handle flips the task to overdue. Idempotent.
func (h MarkOverdueHandler) Handle(ctx context.Context, cmd MarkOverdueCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("tasks mark_overdue: tenant id required")
	}
	if cmd.WorkItemID.IsZero() {
		return errors.New("tasks mark_overdue: work item id required")
	}
	now := h.now()
	err := h.repo.UpdateByID(ctx, cmd.TenantID, cmd.WorkItemID, func(w *workitem.WorkItem) (bool, error) {
		old := w.State()
		if err := w.MarkOverdue(now); err != nil {
			return false, err
		}
		if w.State() == old {
			return false, nil
		}
		return true, nil
	})
	return mapErr(err)
}

// mapErr collapses common domain / repo errors to app-layer sentinels.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workitem.ErrNotFound):
		return ErrWorkItemNotFound
	case errors.Is(err, workitem.ErrConflict):
		return ErrWorkItemTerminal
	default:
		return err
	}
}
