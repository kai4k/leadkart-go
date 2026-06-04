package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// ReassignWorkItemCommand carries a reassign-task request.
type ReassignWorkItemCommand struct {
	TenantID                 tenant.ID
	WorkItemID               workitem.ID
	NewAssigneeMembershipID  string
	ReassignedByMembershipID string
	Reason                   string
}

// ReassignWorkItemHandler runs the hierarchy-gated reassignment flow.
// Per BRD §6.7: the actor (reassigner) may only move a task to a
// membership in their subordinate scope (or themselves). Platform
// callers / SuperUsers short-circuit the check at the HTTP-layer
// (handler decides whether to pass an open SubordinateSet or rely on
// the gate here).
type ReassignWorkItemHandler struct {
	repo        workitem.Repository
	hierarchy   HierarchyReader
	memberships MembershipReader
	now         func() time.Time
}

// NewReassignWorkItemHandler wires the handler.
func NewReassignWorkItemHandler(
	repo workitem.Repository,
	hierarchy HierarchyReader,
	memberships MembershipReader,
	now func() time.Time,
) ReassignWorkItemHandler {
	if repo == nil {
		panic("command: NewReassignWorkItemHandler repo required")
	}
	if hierarchy == nil {
		panic("command: NewReassignWorkItemHandler hierarchy required")
	}
	if memberships == nil {
		panic("command: NewReassignWorkItemHandler memberships required")
	}
	if now == nil {
		now = time.Now
	}
	return ReassignWorkItemHandler{repo: repo, hierarchy: hierarchy, memberships: memberships, now: now}
}

// Handle reassigns the task. Returns [ErrForbiddenReassign] when
// the new assignee is outside the actor's subordinate scope.
func (h ReassignWorkItemHandler) Handle(ctx context.Context, cmd ReassignWorkItemCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("tasks reassign: tenant id required")
	}
	if cmd.WorkItemID.IsZero() {
		return errors.New("tasks reassign: work item id required")
	}
	if cmd.NewAssigneeMembershipID == "" {
		return errors.New("tasks reassign: new assignee membership id required")
	}
	if cmd.ReassignedByMembershipID == "" {
		return errors.New("tasks reassign: reassigned-by membership id required")
	}

	// Validate target is an active membership before hierarchy probe —
	// fast-fail on garbage input.
	exists, err := h.memberships.ExistsActiveInTenant(ctx, cmd.TenantID, cmd.NewAssigneeMembershipID)
	if err != nil {
		return fmt.Errorf("tasks reassign: membership probe: %w", err)
	}
	if !exists {
		return ErrInvalidAssignee
	}

	// Hierarchy gate — actor's subordinate set must include the new
	// assignee. Self-assignment is allowed (subordinate set always
	// includes the actor membership itself per HierarchyReader
	// contract).
	visible, err := h.hierarchy.ListSubordinateMembershipIDs(ctx, cmd.TenantID, cmd.ReassignedByMembershipID)
	if err != nil {
		return fmt.Errorf("tasks reassign: hierarchy lookup: %w", err)
	}
	if !slices.Contains(visible, cmd.NewAssigneeMembershipID) {
		return ErrForbiddenReassign
	}

	now := h.now()
	err = h.repo.UpdateByID(ctx, cmd.TenantID, cmd.WorkItemID, func(w *workitem.WorkItem) (bool, error) {
		old := w.AssignedToMembershipID()
		if err := w.Reassign(cmd.NewAssigneeMembershipID, cmd.ReassignedByMembershipID, cmd.Reason, now); err != nil {
			return false, err
		}
		if w.AssignedToMembershipID() == old {
			return false, nil // idempotent
		}
		return true, nil
	})
	return mapErr(err)
}
