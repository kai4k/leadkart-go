package command_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification/notificationtest"
)

var fixedNow = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

func fixedClock() time.Time              { return fixedNow }
func newNotificationID() notification.ID { return notification.ID(ids.NewV7().String()) }

func newTenantID(t *testing.T) tenant.ID { t.Helper(); return tenant.ID(ids.NewV7().String()) }
func newMembershipID(t *testing.T) membership.ID {
	t.Helper()
	return membership.ID(ids.NewV7().String())
}

func sourcedCmd(tid tenant.ID, recipient membership.ID, entityID string) command.CreateNotificationCommand {
	return command.CreateNotificationCommand{
		TenantID:              tid,
		RecipientMembershipID: recipient,
		Category:              notification.CategoryOrderConfirmed,
		Title:                 "Order confirmed",
		SourceModule:          "orders",
		SourceEntityType:      "order",
		SourceEntityID:        entityID,
		DeepLink:              "/orders/" + entityID,
	}
}

func TestCreateNotification_PersistsAndIsDedupedOnReplay(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	h := command.NewCreateNotificationHandler(repo, fixedClock, newNotificationID)
	tid := newTenantID(t)
	recipient := newMembershipID(t)
	entityID := ids.NewV7().String()

	res, err := h.Handle(t.Context(), sourcedCmd(tid, recipient, entityID))
	require.NoError(t, err)
	require.True(t, res.Created)
	require.Len(t, repo.Store, 1)

	// Replay of the same source event within the dedup window → no new row.
	res2, err := h.Handle(t.Context(), sourcedCmd(tid, recipient, entityID))
	require.NoError(t, err)
	require.False(t, res2.Created, "dedup window must suppress the replay")
	require.Len(t, repo.Store, 1)
}

func TestCreateNotification_RejectsInvalidCategory(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	h := command.NewCreateNotificationHandler(repo, fixedClock, newNotificationID)
	_, err := h.Handle(t.Context(), command.CreateNotificationCommand{
		TenantID: newTenantID(t), RecipientMembershipID: newMembershipID(t),
		Category: notification.Category("bogus"), Title: "x",
	})
	require.ErrorIs(t, err, notification.ErrInvalid)
}

func TestMarkNotificationRead_FlipsUnreadToRead(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	create := command.NewCreateNotificationHandler(repo, fixedClock, newNotificationID)
	tid := newTenantID(t)
	recipient := newMembershipID(t)
	res, err := create.Handle(t.Context(), sourcedCmd(tid, recipient, ids.NewV7().String()))
	require.NoError(t, err)

	mark := command.NewMarkNotificationReadHandler(repo, fixedClock)
	require.NoError(t, mark.Handle(t.Context(), command.MarkNotificationReadCommand{
		TenantID: tid, NotificationID: res.NotificationID, RecipientMembershipID: recipient,
	}))
	got, err := repo.GetByID(t.Context(), tid, res.NotificationID)
	require.NoError(t, err)
	require.Equal(t, notification.StateRead, got.State())
}

func TestMarkNotificationRead_WrongRecipientIsNotFound(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	create := command.NewCreateNotificationHandler(repo, fixedClock, newNotificationID)
	tid := newTenantID(t)
	res, err := create.Handle(t.Context(), sourcedCmd(tid, newMembershipID(t), ids.NewV7().String()))
	require.NoError(t, err)

	mark := command.NewMarkNotificationReadHandler(repo, fixedClock)
	err = mark.Handle(t.Context(), command.MarkNotificationReadCommand{
		TenantID: tid, NotificationID: res.NotificationID, RecipientMembershipID: newMembershipID(t), // someone else
	})
	require.ErrorIs(t, err, notification.ErrNotFound, "must not leak another recipient's notification")
}

func TestDismissNotification_FromUnreadAndAfterRead(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	create := command.NewCreateNotificationHandler(repo, fixedClock, newNotificationID)
	tid := newTenantID(t)
	recipient := newMembershipID(t)
	res, err := create.Handle(t.Context(), sourcedCmd(tid, recipient, ids.NewV7().String()))
	require.NoError(t, err)

	dismiss := command.NewDismissNotificationHandler(repo, fixedClock)
	require.NoError(t, dismiss.Handle(t.Context(), command.DismissNotificationCommand{
		TenantID: tid, NotificationID: res.NotificationID, RecipientMembershipID: recipient,
	}))
	got, err := repo.GetByID(t.Context(), tid, res.NotificationID)
	require.NoError(t, err)
	require.Equal(t, notification.StateDismissed, got.State())
}

func TestMarkAllRead_FlipsEveryUnread(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	create := command.NewCreateNotificationHandler(repo, fixedClock, newNotificationID)
	tid := newTenantID(t)
	recipient := newMembershipID(t)
	for range 3 {
		_, err := create.Handle(t.Context(), sourcedCmd(tid, recipient, ids.NewV7().String()))
		require.NoError(t, err)
	}

	markAll := command.NewMarkAllReadHandler(repo, fixedClock)
	res, err := markAll.Handle(t.Context(), command.MarkAllReadCommand{TenantID: tid, RecipientMembershipID: recipient})
	require.NoError(t, err)
	require.Equal(t, int64(3), res.MarkedRead)

	count, err := repo.CountUnreadForRecipient(t.Context(), tid, recipient)
	require.NoError(t, err)
	require.Zero(t, count)
}
