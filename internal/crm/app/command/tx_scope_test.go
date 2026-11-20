package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

const (
	memberA = "01923400-0000-7000-8000-cccccccc00a1"
	memberB = "01923400-0000-7000-8000-cccccccc00b2"
)

// racingLeads models a reassignment that commits between a caller's scope
// check and its write: UpdateByID first reassigns the lead (as another
// transaction would have), then runs the guard. A handler that decided the
// scope BEFORE the transaction (via GetByID) does not see it.
type racingLeads struct {
	*crmleadtest.FakeRepository
	reassignTo      string
	getByIDCalls    int
	updateByIDCalls int
}

func (r *racingLeads) GetByID(ctx context.Context, tid tenant.ID, id crmlead.ID) (*crmlead.CrmLead, error) {
	r.getByIDCalls++
	return r.FakeRepository.GetByID(ctx, tid, id)
}

func (r *racingLeads) UpdateByID(ctx context.Context, tid tenant.ID, id crmlead.ID, fn func(*crmlead.CrmLead) (bool, error)) error {
	r.updateByIDCalls++
	return r.FakeRepository.UpdateByID(ctx, tid, id, func(l *crmlead.CrmLead) (bool, error) {
		if r.reassignTo != "" {
			if err := l.Assign(r.reassignTo, memberB, "raced", fixedTime()); err != nil {
				return false, err
			}
		}
		return fn(l)
	})
}

func assignedLead(t *testing.T, leads *crmleadtest.FakeRepository, to string) crmlead.ID {
	t.Helper()
	id := seedLead(t, leads)
	if err := leads.UpdateByID(t.Context(), testTenantID, id, func(l *crmlead.CrmLead) (bool, error) {
		return true, l.Assign(to, memberB, "seed", fixedTime())
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	return id
}

func TestLogCall_ScopeIsCheckedInsideTheTransaction(t *testing.T) {
	t.Parallel()
	fake := newFakeLeads()
	id := assignedLead(t, fake, memberA)
	leads := &racingLeads{FakeRepository: fake, reassignTo: memberB}
	calls := newFakeCallLogs()
	h := command.NewLogCallHandler(leads, calls, crmleadtest.InlineTenantUoW{}, fixedTime, newTestCallID)

	_, err := h.Handle(t.Context(), command.LogCallCommand{
		TenantID: testTenantID, LeadID: id, Outcome: calllog.OutcomeInterested,
		LoggedByMembershipID: memberA, OnlyAssignedTo: memberA,
	})
	if !errors.Is(err, command.ErrLeadNotFound) {
		t.Fatalf("a lead reassigned before the write must not exist for its old assignee, got %v", err)
	}
	if rows, _ := calls.ListByLead(t.Context(), testTenantID, id); len(rows) != 0 {
		t.Fatalf("a call was logged on a lead outside the caller's scope: %d rows", len(rows))
	}
	if leads.getByIDCalls != 0 || leads.updateByIDCalls != 1 {
		t.Fatalf("scope must be decided by the locking UpdateByID inside the transaction: GetByID=%d UpdateByID=%d", leads.getByIDCalls, leads.updateByIDCalls)
	}
}

func TestCreateReminder_ScopeIsCheckedInsideTheTransaction(t *testing.T) {
	t.Parallel()
	fake := newFakeLeads()
	id := assignedLead(t, fake, memberA)
	leads := &racingLeads{FakeRepository: fake, reassignTo: memberB}
	reminders := newFakeReminders()
	h := command.NewCreateReminderHandler(leads, reminders, crmleadtest.InlineTenantUoW{}, fixedTime, newTestReminderID)

	_, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: id, Type: reminder.TypeManual,
		AssignedToMembershipID: memberA, CreatedByMembershipID: memberA,
		DueAt: fixedTime().Add(3600e9), OnlyAssignedTo: memberA,
	})
	if !errors.Is(err, command.ErrLeadNotFound) {
		t.Fatalf("a lead reassigned before the write must not exist for its old assignee, got %v", err)
	}
	if leads.getByIDCalls != 0 || leads.updateByIDCalls != 1 {
		t.Fatalf("scope must be decided by the locking UpdateByID inside the transaction: GetByID=%d UpdateByID=%d", leads.getByIDCalls, leads.updateByIDCalls)
	}
}
