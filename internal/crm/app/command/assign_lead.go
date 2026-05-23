package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// ErrLeadNotFound surfaces when the lead ID does not exist in the
// caller's tenant scope (RLS-filtered).
var ErrLeadNotFound = errors.New("crm: lead not found")

// ErrLeadTerminal surfaces when a mutating command targets a lead in a
// terminal stage (converted / lost).
var ErrLeadTerminal = errors.New("crm: lead is in a terminal stage")

// AssignLeadCommand carries a manual-assignment request. AssignedBy is
// the actor (manager / admin) issuing the change; AssigneeMembershipID
// is the new owner.
type AssignLeadCommand struct {
	LeadID                 crmlead.ID
	AssigneeMembershipID   string
	AssignedByMembershipID string
	Reason                 string
}

// AssignLeadResult returns the persisted assignment-history row ID for
// the caller (audit cross-ref).
type AssignLeadResult struct {
	AssignmentID assignmenthistory.ID
}

// AssignLeadHandler runs the manual-assignment flow. Atomically updates
// the lead's current assignee AND writes an assignment-history row in
// the same UoW transaction per ADR 0060 — partial failure rolls back
// both.
type AssignLeadHandler struct {
	leads   crmlead.Repository
	history assignmenthistory.Repository
	uow     pg.UnitOfWork
	now     func() time.Time
}

// NewAssignLeadHandler wires the handler against domain interfaces +
// the UoW primitive.
func NewAssignLeadHandler(leads crmlead.Repository, history assignmenthistory.Repository, uow pg.UnitOfWork, now func() time.Time) AssignLeadHandler {
	if leads == nil {
		panic("command: NewAssignLeadHandler leads repository required")
	}
	if history == nil {
		panic("command: NewAssignLeadHandler history repository required")
	}
	if uow == nil {
		panic("command: NewAssignLeadHandler uow required")
	}
	if now == nil {
		now = time.Now
	}
	return AssignLeadHandler{leads: leads, history: history, uow: uow, now: now}
}

// Handle performs the assignment + audit write in one transaction.
func (h AssignLeadHandler) Handle(ctx context.Context, cmd AssignLeadCommand) (AssignLeadResult, error) {
	if cmd.LeadID.IsZero() {
		return AssignLeadResult{}, errors.New("crm assign: lead id required")
	}
	if cmd.AssigneeMembershipID == "" {
		return AssignLeadResult{}, errors.New("crm assign: assignee membership id required")
	}
	if cmd.AssignedByMembershipID == "" {
		return AssignLeadResult{}, errors.New("crm assign: assigned-by membership id required")
	}

	var resultID assignmenthistory.ID
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		var captured struct {
			previous string
			tenant   string
			noop     bool
		}
		err := h.leads.UpdateByID(ctx, cmd.LeadID, func(l *crmlead.CrmLead) (bool, error) {
			captured.previous = l.AssigneeMembershipID()
			captured.tenant = l.TenantID()
			if err := l.Assign(cmd.AssigneeMembershipID, cmd.AssignedByMembershipID, cmd.Reason); err != nil {
				return false, err
			}
			// idempotent self-assignment: aggregate emitted no event.
			if l.AssigneeMembershipID() == captured.previous {
				captured.noop = true
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		if captured.noop {
			return nil
		}
		entry, err := assignmenthistory.New(
			assignmenthistory.ID(ids.NewV7().String()),
			captured.tenant,
			cmd.LeadID,
			captured.previous,
			cmd.AssigneeMembershipID,
			cmd.AssignedByMembershipID,
			cmd.Reason,
			h.now(),
		)
		if err != nil {
			return fmt.Errorf("crm assign: history factory: %w", err)
		}
		if err := h.history.Add(ctx, entry); err != nil {
			return fmt.Errorf("crm assign: history persist: %w", err)
		}
		resultID = entry.ID()
		return nil
	})
	if err != nil {
		if errors.Is(err, crmlead.ErrNotFound) {
			return AssignLeadResult{}, ErrLeadNotFound
		}
		if errors.Is(err, crmlead.ErrTerminal) {
			return AssignLeadResult{}, ErrLeadTerminal
		}
		return AssignLeadResult{}, err
	}
	return AssignLeadResult{AssignmentID: resultID}, nil
}
