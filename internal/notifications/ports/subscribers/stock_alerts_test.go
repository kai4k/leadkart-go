package subscribers_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryevents "github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
	"github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification/notificationtest"
	"github.com/leadkart/leadkart-go/internal/notifications/ports/subscribers"
)

type fixedRecipients struct {
	people []membership.ID
	err    error
}

func (f fixedRecipients) StockAlertRecipients(context.Context, tenant.ID) ([]membership.ID, error) {
	return f.people, f.err
}

var alertNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func newSub(repo *notificationtest.FakeRepository, r command.StockAlertRecipientResolver) *subscribers.StockAlertSubscriber {
	create := command.NewCreateNotificationHandler(repo, func() time.Time { return alertNow }, func() notification.ID { return notification.ID(ids.NewV7().String()) })
	return subscribers.NewStockAlertSubscriber(create, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestStockAlerts_AnExpiryAlertReachesEveryRecipient(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	tid := uuid.New()
	a, b := membership.ID(ids.NewV7().String()), membership.ID(ids.NewV7().String())
	sub := newSub(repo, fixedRecipients{people: []membership.ID{a, b}})

	evt := &inventoryevents.BatchExpiringSoonV1{
		BatchID: uuid.New(), ProductID: uuid.New(), TenantIDClaim: tid, BatchNumber: "LOT-9", ProductSKU: "AMOX-500", ProductName: "Amoxicillin",
		ExpiryDate: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), LastSellableDay: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		DaysUntilExpiry: 68, TierDays: 90, FreeQuantity: 80, OccurredAtUTC: alertNow,
	}
	if err := sub.HandleExpiring(t.Context(), evt); err != nil {
		t.Fatal(err)
	}
	for _, m := range []membership.ID{a, b} {
		page, err := repo.ListPageForRecipient(t.Context(), tenant.ID(tid.String()), m, notification.ListFilter{}, pagination.Cursor{}, 50)
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("recipient %s: %d notifications, err %v", m, len(page.Items), err)
		}
		n := page.Items[0]
		if n.Category() != notification.CategoryStockExpiring || !strings.Contains(n.Title(), "AMOX-500") || !strings.Contains(n.Title(), "68 days") {
			t.Fatalf("notification %q %q", n.Category(), n.Title())
		}
		if !strings.Contains(n.Body(), "31 Jul 2026") {
			t.Fatalf("body %q must carry the last sellable day", n.Body())
		}
	}
}

func TestStockAlerts_ExpiredStockAndReorderAreDelivered(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	tid := uuid.New()
	m := membership.ID(ids.NewV7().String())
	sub := newSub(repo, fixedRecipients{people: []membership.ID{m}})

	if err := sub.HandleExpiredOnHand(t.Context(), &inventoryevents.BatchExpiredOnHandV1{
		BatchID: uuid.New(), ProductID: uuid.New(), TenantIDClaim: tid, BatchNumber: "OLD", ProductSKU: "S1", ProductName: "Drug",
		ExpiryDate: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), Quantity: 20, OccurredAtUTC: alertNow,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sub.HandleBelowReorder(t.Context(), &inventoryevents.ProductBelowReorderLevelV1{
		ProductID: uuid.New(), TenantIDClaim: tid, SKU: "S2", ReorderLevel: 100, CurrentStockOnHand: 25, OccurredAtUTC: alertNow,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := repo.ListPageForRecipient(t.Context(), tenant.ID(tid.String()), m, notification.ListFilter{}, pagination.Cursor{}, 50)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("%d notifications, err %v", len(page.Items), err)
	}
	cats := map[notification.Category]bool{}
	for _, n := range page.Items {
		cats[n.Category()] = true
	}
	if !cats[notification.CategoryStockExpiring] || !cats[notification.CategoryStockBelowReorder] {
		t.Fatalf("categories %v", cats)
	}
}

func TestStockAlerts_NoRecipientIsAcknowledgedAndARecipientFailureIsRetried(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	evt := &inventoryevents.ProductBelowReorderLevelV1{ProductID: uuid.New(), TenantIDClaim: uuid.New(), SKU: "S", ReorderLevel: 1, OccurredAtUTC: alertNow}
	if err := newSub(repo, fixedRecipients{}).HandleBelowReorder(t.Context(), evt); err != nil {
		t.Fatalf("a company with nobody to tell must not retry forever: %v", err)
	}
	if err := newSub(repo, fixedRecipients{err: errors.New("identity down")}).HandleBelowReorder(t.Context(), evt); err == nil {
		t.Fatal("a recipient lookup failure must be retried")
	}
}

func TestHandlers_RegistersTheStockAlertHandlersUnderStableNames(t *testing.T) {
	t.Parallel()
	sub := newSub(notificationtest.NewFakeRepository(), fixedRecipients{})
	hs := subscribers.StockAlertHandlers(sub)
	got := map[string]bool{}
	for _, h := range hs {
		got[h.HandlerName()] = true
	}
	for _, want := range []string{"notifications.subscribers.BatchExpiringSoon", "notifications.subscribers.BatchExpiredOnHand", "notifications.subscribers.ProductBelowReorderLevel"} {
		if !got[want] {
			t.Fatalf("handlers %v: missing %s", got, want)
		}
	}
}
