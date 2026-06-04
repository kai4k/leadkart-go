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

func TestCreateWorkItem_Success(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	actor := uuid.New().String()
	assignee := uuid.New().String()
	wid := workitem.ID(uuid.New().String())

	repo := workitemtest.NewFakeRepository()
	memberships := newFakeMemberships(assignee)
	h := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now },
		func() workitem.ID { return wid })

	out, err := h.Handle(t.Context(), command.CreateWorkItemCommand{
		TenantID:               tenantID,
		Type:                   workitem.TypeManual,
		Priority:               workitem.PriorityMedium,
		Title:                  "Test",
		AssignedToMembershipID: assignee,
		AssignedByMembershipID: actor,
		DueAt:                  now.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, wid, out.WorkItemID)

	got, ok := repo.ByID[wid]
	require.True(t, ok)
	require.Equal(t, workitem.StatePending, got.State())
	require.Equal(t, assignee, got.AssignedToMembershipID())

	evs := repo.EmittedEventsByID[wid]
	require.Len(t, evs, 1)
	_, ok = evs[0].(workitem.CreatedEvent)
	require.True(t, ok)
}

func TestCreateWorkItem_RejectsInactiveAssignee(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	actor := uuid.New().String()
	assignee := uuid.New().String()

	repo := workitemtest.NewFakeRepository()
	memberships := newFakeMemberships() // empty — assignee not active
	h := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now },
		func() workitem.ID { return workitem.ID(uuid.New().String()) })

	_, err := h.Handle(t.Context(), command.CreateWorkItemCommand{
		TenantID:               tenantID,
		Type:                   workitem.TypeManual,
		Title:                  "Test",
		AssignedToMembershipID: assignee,
		AssignedByMembershipID: actor,
		DueAt:                  now.Add(time.Hour),
	})
	require.ErrorIs(t, err, command.ErrInvalidAssignee)
}

func TestCreateWorkItem_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())

	repo := workitemtest.NewFakeRepository()
	memberships := newFakeMemberships()
	h := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now },
		func() workitem.ID { return workitem.ID(uuid.New().String()) })

	_, err := h.Handle(t.Context(), command.CreateWorkItemCommand{
		TenantID:               tenant.ID(""),
		AssignedToMembershipID: "x",
		AssignedByMembershipID: "y",
	})
	require.Error(t, err)

	_, err = h.Handle(t.Context(), command.CreateWorkItemCommand{
		TenantID: tenantID,
	})
	require.Error(t, err)
}

func TestStartCompleteCancel_FullCycle(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	actor := uuid.New().String()
	assignee := uuid.New().String()
	wid := workitem.ID(uuid.New().String())

	repo := workitemtest.NewFakeRepository()
	memberships := newFakeMemberships(assignee)
	create := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now }, func() workitem.ID { return wid })
	_, err := create.Handle(t.Context(), command.CreateWorkItemCommand{
		TenantID: tenantID, Type: workitem.TypeManual, Title: "T",
		AssignedToMembershipID: assignee, AssignedByMembershipID: actor,
		DueAt: now.Add(time.Hour),
	})
	require.NoError(t, err)

	start := command.NewStartWorkItemHandler(repo, func() time.Time { return now })
	require.NoError(t, start.Handle(t.Context(), command.StartWorkItemCommand{
		TenantID: tenantID, WorkItemID: wid, ActorID: actor,
	}))
	require.Equal(t, workitem.StateInProgress, repo.ByID[wid].State())

	complete := command.NewCompleteWorkItemHandler(repo, func() time.Time { return now })
	require.NoError(t, complete.Handle(t.Context(), command.CompleteWorkItemCommand{
		TenantID: tenantID, WorkItemID: wid, ActorID: actor,
	}))
	require.Equal(t, workitem.StateCompleted, repo.ByID[wid].State())
}

func TestCancel_RequiresReason(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	actor := uuid.New().String()
	assignee := uuid.New().String()
	wid := workitem.ID(uuid.New().String())

	repo := workitemtest.NewFakeRepository()
	memberships := newFakeMemberships(assignee)
	_, err := command.NewCreateWorkItemHandler(repo, memberships,
		func() time.Time { return now }, func() workitem.ID { return wid }).
		Handle(t.Context(), command.CreateWorkItemCommand{
			TenantID: tenantID, Type: workitem.TypeManual, Title: "T",
			AssignedToMembershipID: assignee, AssignedByMembershipID: actor,
			DueAt: now.Add(time.Hour),
		})
	require.NoError(t, err)

	cancel := command.NewCancelWorkItemHandler(repo, func() time.Time { return now })
	err = cancel.Handle(t.Context(), command.CancelWorkItemCommand{
		TenantID: tenantID, WorkItemID: wid, ActorID: actor, Reason: "",
	})
	require.Error(t, err)
}

func TestMarkOverdue_NotFound(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	repo := workitemtest.NewFakeRepository()
	h := command.NewMarkOverdueHandler(repo, func() time.Time { return now })
	err := h.Handle(t.Context(), command.MarkOverdueCommand{
		TenantID: tenantID, WorkItemID: workitem.ID(uuid.New().String()),
	})
	require.ErrorIs(t, err, command.ErrWorkItemNotFound)
}
