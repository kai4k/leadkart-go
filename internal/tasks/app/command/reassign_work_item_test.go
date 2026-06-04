package command_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/app/command"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem/workitemtest"
)

func TestReassign_Success_TargetInSubordinateScope(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	manager := uuid.New().String()
	subA := uuid.New().String()
	subB := uuid.New().String()

	hierarchy := &fakeHierarchy{Subordinates: map[string][]string{
		manager: {manager, subA, subB},
	}}
	memberships := newFakeMemberships(manager, subA, subB)
	repo := workitemtest.NewFakeRepository()

	wid := workitem.ID(uuid.New().String())
	_, err := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now }, func() workitem.ID { return wid }).
		Handle(t.Context(), command.CreateWorkItemCommand{
			TenantID: tenantID, Type: workitem.TypeManual, Title: "T",
			AssignedToMembershipID: subA, AssignedByMembershipID: manager,
			DueAt: now.Add(time.Hour),
		})
	require.NoError(t, err)

	h := command.NewReassignWorkItemHandler(repo, hierarchy, memberships, func() time.Time { return now })
	err = h.Handle(t.Context(), command.ReassignWorkItemCommand{
		TenantID: tenantID, WorkItemID: wid, NewAssigneeMembershipID: subB,
		ReassignedByMembershipID: manager, Reason: "rebalance",
	})
	require.NoError(t, err)
	require.Equal(t, subB, repo.ByID[wid].AssignedToMembershipID())
}

func TestReassign_ForbiddenWhenTargetOutOfScope(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	managerA := uuid.New().String()
	managerB := uuid.New().String()
	subOfB := uuid.New().String()

	hierarchy := &fakeHierarchy{Subordinates: map[string][]string{
		managerA: {managerA},
		managerB: {managerB, subOfB},
	}}
	memberships := newFakeMemberships(managerA, managerB, subOfB)
	repo := workitemtest.NewFakeRepository()

	wid := workitem.ID(uuid.New().String())
	_, err := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now }, func() workitem.ID { return wid }).
		Handle(t.Context(), command.CreateWorkItemCommand{
			TenantID: tenantID, Type: workitem.TypeManual, Title: "T",
			AssignedToMembershipID: managerA, AssignedByMembershipID: managerA,
			DueAt: now.Add(time.Hour),
		})
	require.NoError(t, err)

	// managerA tries to reassign to subOfB (in managerB's chain) → forbidden.
	h := command.NewReassignWorkItemHandler(repo, hierarchy, memberships, func() time.Time { return now })
	err = h.Handle(t.Context(), command.ReassignWorkItemCommand{
		TenantID: tenantID, WorkItemID: wid, NewAssigneeMembershipID: subOfB,
		ReassignedByMembershipID: managerA,
	})
	require.ErrorIs(t, err, command.ErrForbiddenReassign)
}

func TestReassign_RejectsInactiveTarget(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	manager := uuid.New().String()
	missing := uuid.New().String()
	other := uuid.New().String()

	hierarchy := &fakeHierarchy{Subordinates: map[string][]string{
		manager: {manager, other},
	}}
	memberships := newFakeMemberships(manager, other) // missing is not active

	repo := workitemtest.NewFakeRepository()
	wid := workitem.ID(uuid.New().String())
	_, err := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now }, func() workitem.ID { return wid }).
		Handle(t.Context(), command.CreateWorkItemCommand{
			TenantID: tenantID, Type: workitem.TypeManual, Title: "T",
			AssignedToMembershipID: other, AssignedByMembershipID: manager,
			DueAt: now.Add(time.Hour),
		})
	require.NoError(t, err)

	h := command.NewReassignWorkItemHandler(repo, hierarchy, memberships, func() time.Time { return now })
	err = h.Handle(t.Context(), command.ReassignWorkItemCommand{
		TenantID: tenantID, WorkItemID: wid, NewAssigneeMembershipID: missing,
		ReassignedByMembershipID: manager,
	})
	require.ErrorIs(t, err, command.ErrInvalidAssignee)
}
