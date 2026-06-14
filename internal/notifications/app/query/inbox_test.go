package query_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/app/query"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification/notificationtest"
)

var fixedNow = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

func seed(t *testing.T, repo *notificationtest.FakeRepository, tid tenant.ID, recipient membership.ID, n int) {
	t.Helper()
	for i := range n {
		notif, err := notification.New(notification.NewInput{
			ID:                    notification.ID(ids.NewV7().String()),
			TenantID:              tid,
			RecipientMembershipID: recipient,
			Category:              notification.CategoryLeadAssigned,
			Title:                 "Lead assigned",
			SourceModule:          "crm",
			SourceEntityType:      "lead",
			SourceEntityID:        ids.NewV7().String(),
			Now:                   fixedNow.Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
		require.NoError(t, repo.Add(t.Context(), notif))
	}
}

func TestListInbox_ReturnsRecipientViewsOnly(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	tid := tenant.ID(ids.NewV7().String())
	mine := membership.ID(ids.NewV7().String())
	other := membership.ID(ids.NewV7().String())
	seed(t, repo, tid, mine, 2)
	seed(t, repo, tid, other, 3)

	h := query.NewListInboxHandler(repo)
	page, err := h.Handle(t.Context(), query.ListInboxQuery{
		TenantID: tid, RecipientMembershipID: mine, PageSize: 50,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	for _, v := range page.Items {
		require.Equal(t, "lead_assigned", v.Category)
		require.Equal(t, "unread", v.State)
	}
}

func TestUnreadCount_CountsOnlyUnreadForRecipient(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	tid := tenant.ID(ids.NewV7().String())
	mine := membership.ID(ids.NewV7().String())
	seed(t, repo, tid, mine, 4)

	h := query.NewUnreadCountHandler(repo)
	n, err := h.Handle(t.Context(), query.UnreadCountQuery{TenantID: tid, RecipientMembershipID: mine})
	require.NoError(t, err)
	require.Equal(t, int64(4), n)
}
