// Package command holds Tasks command handlers per TDL canon. Each
// handler has a unique HandlerXxx struct + Handle(ctx, cmd) method.
package command

import "errors"

// ErrWorkItemNotFound surfaces when the work item ID does not exist
// in the caller's tenant scope (RLS-filtered).
var ErrWorkItemNotFound = errors.New("tasks: work item not found")

// ErrWorkItemTerminal surfaces when a mutating command targets a
// task in a terminal state (completed / cancelled).
var ErrWorkItemTerminal = errors.New("tasks: work item is terminal")

// ErrForbiddenReassign surfaces when a reassign command's actor
// lacks the hierarchy authority to move the task to the requested
// new assignee (BRD §6.7 visibility rule).
var ErrForbiddenReassign = errors.New("tasks: reassign target outside actor's subordinate scope")

// ErrInvalidAssignee surfaces when the assignee target membership is
// not active in the tenant (or doesn't exist).
var ErrInvalidAssignee = errors.New("tasks: assignee membership not active in tenant")
