//go:build integration

// arch-test:no-timeout-needed — shared pgtest container; each case is a handful
// of short tenant-scoped txs on one or two notification rows.

package adapters_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/adapters"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

func newNotificationRepo(t *testing.T) *adapters.NotificationRepository {
	t.Helper()
	pool := repoFixture(t)
	return adapters.NewNotificationRepository(pool, pg.NewTransactor(pool))
}

func sourcedNotification(t *testing.T, tid tenant.ID, recipient membership.ID, createdBy membership.ID, at time.Time) *notification.Notification {
	t.Helper()
	n, err := notification.New(notification.NewInput{
		ID:                    notification.ID(uuid.NewString()),
		TenantID:              tid,
		RecipientMembershipID: recipient,
		Category:              notification.CategoryOrderConfirmed,
		Title:                 "Order confirmed",
		Body:                  "Your order is set",
		SourceModule:          "orders",
		SourceEntityType:      "order",
		SourceEntityID:        uuid.NewString(),
		DeepLink:              "/orders/x",
		CreatedByMembershipID: createdBy,
		Now:                   at,
	})
	require.NoError(t, err)
	return n
}

// TestNotificationRepository_RoundTripAndInbox pins Add → GetByID field survival
// (incl. nullable created_by mapping), the recipient inbox list, and unread count.
func TestNotificationRepository_RoundTripAndInbox(t *testing.T) {
	repo := newNotificationRepo(t)
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	recipient := membership.ID(uuid.NewString())
	createdBy := membership.ID(uuid.NewString())

	n := sourcedNotification(t, tid, recipient, createdBy, fixedNow)
	require.NoError(t, repo.Add(ctx, n))

	got, err := repo.GetByID(ctx, tid, n.ID())
	require.NoError(t, err)
	require.Equal(t, notification.StateUnread, got.State())
	require.Equal(t, notification.CategoryOrderConfirmed, got.Category())
	require.Equal(t, "Order confirmed", got.Title())
	require.Equal(t, createdBy, got.CreatedByMembershipID())

	page, err := repo.ListPageForRecipient(ctx, tid, recipient, notification.ListFilter{}, pagination.Cursor{}, 50)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)

	count, err := repo.CountUnreadForRecipient(ctx, tid, recipient)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestNotificationRepository_NullCreatedByRoundTrips pins the system/cron path:
// a notification with no human creator stores + reloads created_by as zero.
func TestNotificationRepository_NullCreatedByRoundTrips(t *testing.T) {
	repo := newNotificationRepo(t)
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	recipient := membership.ID(uuid.NewString())

	n := sourcedNotification(t, tid, recipient, membership.ID(""), fixedNow)
	require.NoError(t, repo.Add(ctx, n))

	got, err := repo.GetByID(ctx, tid, n.ID())
	require.NoError(t, err)
	require.Equal(t, membership.ID(""), got.CreatedByMembershipID(), "nil created_by must round-trip as zero")
}

// TestNotificationRepository_DedupWindowSuppressesReplay pins the at-least-once
// safety: a second sourced Add for the same (recipient, source, category) inside
// the window is rejected with ErrDuplicateInDedupWindow.
func TestNotificationRepository_DedupWindowSuppressesReplay(t *testing.T) {
	repo := newNotificationRepo(t)
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	recipient := membership.ID(uuid.NewString())

	first, err := notification.New(notification.NewInput{
		ID: notification.ID(uuid.NewString()), TenantID: tid, RecipientMembershipID: recipient,
		Category: notification.CategoryLeadAssigned, Title: "Lead assigned",
		SourceModule: "crm", SourceEntityType: "lead", SourceEntityID: "lead-123",
		Now: fixedNow,
	})
	require.NoError(t, err)
	require.NoError(t, repo.Add(ctx, first))

	// Same source + recipient + category, 1 minute later (inside 5-min window).
	dup, err := notification.New(notification.NewInput{
		ID: notification.ID(uuid.NewString()), TenantID: tid, RecipientMembershipID: recipient,
		Category: notification.CategoryLeadAssigned, Title: "Lead assigned (replay)",
		SourceModule: "crm", SourceEntityType: "lead", SourceEntityID: "lead-123",
		Now: fixedNow.Add(1 * time.Minute),
	})
	require.NoError(t, err)
	require.ErrorIs(t, repo.Add(ctx, dup), notification.ErrDuplicateInDedupWindow)

	count, err := repo.CountUnreadForRecipient(ctx, tid, recipient)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "replay must not create a second row")
}

// TestNotificationRepository_MarkReadAndMarkAll pins the state transitions:
// UpdateByID flips one to read; UpdateAllUnreadForRecipient bulk-clears.
func TestNotificationRepository_MarkReadAndMarkAll(t *testing.T) {
	repo := newNotificationRepo(t)
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	recipient := membership.ID(uuid.NewString())

	one := sourcedNotification(t, tid, recipient, membership.ID(""), fixedNow)
	two := sourcedNotification(t, tid, recipient, membership.ID(""), fixedNow.Add(time.Minute))
	require.NoError(t, repo.Add(ctx, one))
	require.NoError(t, repo.Add(ctx, two))

	require.NoError(t, repo.UpdateByID(ctx, tid, one.ID(), func(n *notification.Notification) (bool, error) {
		return true, n.MarkRead(fixedNow.Add(2 * time.Minute))
	}))
	got, err := repo.GetByID(ctx, tid, one.ID())
	require.NoError(t, err)
	require.Equal(t, notification.StateRead, got.State())
	require.NotNil(t, got.ReadAt())

	affected, err := repo.UpdateAllUnreadForRecipient(ctx, tid, recipient, fixedNow.Add(3*time.Minute).UnixNano())
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "only the remaining unread one flips")

	count, err := repo.CountUnreadForRecipient(ctx, tid, recipient)
	require.NoError(t, err)
	require.Zero(t, count)
}
