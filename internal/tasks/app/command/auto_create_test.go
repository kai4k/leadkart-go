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

func TestAutoCreateFromCallLog_FreshAndIdempotent(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	logger := uuid.New().String()
	callID := uuid.New().String()
	wid := workitem.ID(uuid.New().String())

	repo := workitemtest.NewFakeRepository()
	count := 0
	h := command.NewAutoCreateFromCallLogHandler(repo, func() time.Time { return now },
		func() workitem.ID {
			count++
			if count == 1 {
				return wid
			}
			return workitem.ID(uuid.New().String())
		})

	out, err := h.Handle(t.Context(), command.AutoCreateFromCallLogCommand{
		TenantID: tenantID, CallLogID: callID, LoggedByMembershipID: logger,
		CallbackAt: now.Add(2 * time.Hour), LeadContactName: "Pharma X",
	})
	require.NoError(t, err)
	require.Equal(t, wid, out.WorkItemID)
	require.False(t, out.AlreadyExisted)

	// Replay — should hit the source-uniqueness idempotency path.
	out2, err := h.Handle(t.Context(), command.AutoCreateFromCallLogCommand{
		TenantID: tenantID, CallLogID: callID, LoggedByMembershipID: logger,
		CallbackAt: now.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	require.True(t, out2.AlreadyExisted)
	require.Equal(t, wid, out2.WorkItemID)
}

func TestAutoCompleteBySource_NoMatch(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	actor := uuid.New().String()

	repo := workitemtest.NewFakeRepository()
	h := command.NewAutoCompleteBySourceHandler(repo, func() time.Time { return now })
	out, err := h.Handle(t.Context(), command.AutoCompleteBySourceCommand{
		TenantID:         tenantID,
		SourceEntityType: "call_log",
		SourceEntityID:   uuid.New().String(),
		ActorID:          actor,
	})
	require.NoError(t, err)
	require.True(t, out.NoMatch)
}

func TestAutoCompleteBySource_CompletesExisting(t *testing.T) {
	t.Parallel()
	now := fixedNow
	tenantID := tenant.ID(uuid.New().String())
	logger := uuid.New().String()
	callID := uuid.New().String()
	wid := workitem.ID(uuid.New().String())

	repo := workitemtest.NewFakeRepository()
	_, err := command.NewAutoCreateFromCallLogHandler(repo, func() time.Time { return now },
		func() workitem.ID { return wid }).
		Handle(t.Context(), command.AutoCreateFromCallLogCommand{
			TenantID: tenantID, CallLogID: callID, LoggedByMembershipID: logger,
			CallbackAt: now.Add(time.Hour),
		})
	require.NoError(t, err)

	h := command.NewAutoCompleteBySourceHandler(repo, func() time.Time { return now })
	out, err := h.Handle(t.Context(), command.AutoCompleteBySourceCommand{
		TenantID:         tenantID,
		SourceEntityType: "call_log",
		SourceEntityID:   callID,
		ActorID:          logger,
	})
	require.NoError(t, err)
	require.False(t, out.NoMatch)
	require.Equal(t, wid, out.WorkItemID)
	require.Equal(t, workitem.StateCompleted, repo.ByID[wid].State())
}
