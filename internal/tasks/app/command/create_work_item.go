package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// CreateWorkItemCommand carries a manual create-task request from
// the HTTP handler.
type CreateWorkItemCommand struct {
	TenantID               tenant.ID
	Type                   workitem.Type
	Priority               workitem.Priority
	Title                  string
	Description            string
	AssignedToMembershipID string
	AssignedByMembershipID string // the actor; the HTTP layer pulls from JWT
	BatchID                string
	DueAt                  time.Time
}

// CreateWorkItemResult returns the new work-item ID.
type CreateWorkItemResult struct {
	WorkItemID workitem.ID
}

// CreateWorkItemHandler runs the create-task flow.
type CreateWorkItemHandler struct {
	repo          workitem.Repository
	memberships   MembershipReader
	now           func() time.Time
	newWorkItemID func() workitem.ID
}

// NewCreateWorkItemHandler wires the handler.
//
// newWorkItemID is the WorkItem ID factory per the
// TestArch_HandlersInjectIDFactory discipline. Production passes
// `func() workitem.ID { return workitem.ID(ids.NewV7().String()) }`;
// tests inject a deterministic counter so the minted ID is pinnable.
func NewCreateWorkItemHandler(
	repo workitem.Repository,
	memberships MembershipReader,
	now func() time.Time,
	newWorkItemID func() workitem.ID,
) CreateWorkItemHandler {
	if repo == nil {
		panic("command: NewCreateWorkItemHandler repo required")
	}
	if memberships == nil {
		panic("command: NewCreateWorkItemHandler memberships required")
	}
	if newWorkItemID == nil {
		panic("command: NewCreateWorkItemHandler newWorkItemID required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateWorkItemHandler{repo: repo, memberships: memberships, now: now, newWorkItemID: newWorkItemID}
}

// Handle persists the work item + emits the V1 event via the
// repository's outbox drain.
func (h CreateWorkItemHandler) Handle(ctx context.Context, cmd CreateWorkItemCommand) (CreateWorkItemResult, error) {
	if cmd.TenantID.IsZero() {
		return CreateWorkItemResult{}, errors.New("tasks create: tenant id required")
	}
	if cmd.AssignedToMembershipID == "" {
		return CreateWorkItemResult{}, errors.New("tasks create: assignee membership id required")
	}
	if cmd.AssignedByMembershipID == "" {
		return CreateWorkItemResult{}, errors.New("tasks create: assigned-by membership id required")
	}
	// Active-membership check — refuse creating tasks targeting a
	// deactivated / non-existent membership in the tenant.
	ok, err := h.memberships.ExistsActiveInTenant(ctx, cmd.TenantID, cmd.AssignedToMembershipID)
	if err != nil {
		return CreateWorkItemResult{}, fmt.Errorf("tasks create: membership probe: %w", err)
	}
	if !ok {
		return CreateWorkItemResult{}, ErrInvalidAssignee
	}

	now := h.now()
	w, err := workitem.NewManual(workitem.NewParams{
		ID:                     h.newWorkItemID(),
		TenantID:               cmd.TenantID,
		Type:                   cmd.Type,
		Priority:               cmd.Priority,
		Title:                  cmd.Title,
		Description:            cmd.Description,
		AssignedToMembershipID: cmd.AssignedToMembershipID,
		AssignedByMembershipID: cmd.AssignedByMembershipID,
		CreatedByMembershipID:  cmd.AssignedByMembershipID, // creator = the actor
		DueAt:                  cmd.DueAt,
		BatchID:                cmd.BatchID,
		Now:                    now,
	})
	if err != nil {
		return CreateWorkItemResult{}, fmt.Errorf("tasks create: factory: %w", err)
	}
	if err := h.repo.Add(ctx, w); err != nil {
		return CreateWorkItemResult{}, fmt.Errorf("tasks create: persist: %w", err)
	}
	return CreateWorkItemResult{WorkItemID: w.ID()}, nil
}
