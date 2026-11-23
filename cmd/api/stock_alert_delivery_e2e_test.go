//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(); the
// polls are bounded by waitFor (15s).
//
// arch-test:no-synctest — the alert travels scan -> outbox -> forwarder -> router
// -> notifications subscriber across SQL and HTTP, which synctest cannot model.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	identityadapters "github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventoryjobs "github.com/leadkart/leadkart-go/internal/inventory/app/jobs"
	inventorysellability "github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	notificationsadapters "github.com/leadkart/leadkart-go/internal/notifications/adapters"
	notificationscommand "github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	notificationssubscribers "github.com/leadkart/leadkart-go/internal/notifications/ports/subscribers"
)

// holdersRecipients is the bridge cmd/worker binds: Identity's permission holders.
type holdersRecipients struct {
	holders *identityadapters.PermissionHolderReader
}

func (r holdersRecipients) StockAlertRecipients(ctx context.Context, tenantID tenant.ID) ([]membership.ID, error) {
	p := permission.IdentityPermissions
	return r.holders.ActiveMembershipsHoldingAny(ctx, tenantID, []string{p.Meta.TenantAdmin, p.Inventory.Stock.Manage})
}

// inbox returns the caller own notifications.
func inbox(t *testing.T, f e2eFixture, tok string) []struct{ Category, Title, Body, DeepLink string } {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/notifications?page_size=100", tok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("inbox: %d %s", r.status, r.body)
	}
	var page struct {
		Items []struct {
			Category string `json:"category"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			DeepLink string `json:"deep_link"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.body, &page); err != nil {
		t.Fatal(err)
	}
	out := make([]struct{ Category, Title, Body, DeepLink string }, 0, len(page.Items))
	for _, it := range page.Items {
		out = append(out, struct{ Category, Title, Body, DeepLink string }{it.Category, it.Title, it.Body, it.DeepLink})
	}
	return out
}

// Required proof of G2: an expiry alert (and a reorder alert and an
// expired-on-hand alert) reaches a REAL user's inbox through the whole path: scan,
// outbox, forwarder, router, Notifications, the recipient resolved from Identity.
func TestE2E_StockAlerts_AnExpiryAlertReachesTheCompanyAdministrator(t *testing.T) {
	f := newE2EFixture(t)
	tx := pg.NewTransactor(f.Pool)
	log := silentLogger()

	repo := notificationsadapters.NewNotificationRepository(f.Pool, tx)
	create := notificationscommand.NewCreateNotificationHandler(repo, time.Now, func() notification.ID { return notification.ID(uuid.NewString()) })
	sub := notificationssubscribers.NewStockAlertSubscriber(create, holdersRecipients{holders: identityadapters.NewPermissionHolderReader(f.Pool, tx)}, log)
	stack := policyStack{pool: f.Pool, tx: tx, pubsub: messaging.NewPubSub(watermill.NewSlogLogger(log))}
	t.Cleanup(func() { _ = stack.pubsub.Close() })
	stack.runRouter(t, []cqrs.EventHandler(notificationssubscribers.StockAlertHandlers(sub)))
	forwarder, err := messaging.NewOutboxForwarder(f.Pool, stack.pubsub, watermill.NewSlogLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		if err := forwarder.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("forwarder: %v", err)
		}
	}()
	t.Cleanup(func() { _ = forwarder.Close() })

	reg := f.registerAndLogin(t, "stockalert")
	tok := reg.AccessToken
	tid := uuid.MustParse(reg.TenantID)
	now := time.Now().UTC()
	month := func(plus int) time.Time { return expiry.MonthEnd(now.Year(), now.Month()+time.Month(plus)) }

	// Stock expiring: printed two months ahead, so the last sellable day (the default stop is
	// the start of the printed month) is the end of next month: 31 to 62 days away, the 90-day tier.
	p := invCreateProduct(t, f, tok, "ALERT-1", map[string]any{"reorder_level": 100})
	if r := invAddBatch(t, f, tok, p, "SOON", month(2), map[string]any{"initial_quantity": 10}); r.status != http.StatusCreated {
		t.Fatalf("add batch: %d %s", r.status, r.body)
	}
	// Expired stock not set apart: an empty batch in a past printed month, stocked by an adjustment.
	old := invAddBatch(t, f, tok, p, "OLD", month(-1), nil)
	oldID, _ := invObj(t, old.body)["batch_id"].(string)
	if r := invMove(t, f, tok, oldID, "adjustment", 5); r.status != http.StatusCreated {
		t.Fatalf("adjustment: %d %s", r.status, r.body)
	}

	replica := inventoryadapters.NewPolicyReplicaRepository(f.Pool, tx)
	scanner := inventoryjobs.NewScanner(inventoryadapters.NewAlertScanRepository(f.Pool, tx),
		inventorysellability.NewResolver(replica, time.Now), log, time.Now)
	n, err := scanner.ScanExpiry(t.Context(), tid)
	if err != nil || n != 2 {
		t.Fatalf("first expiry scan raised %d alerts, err %v; want 2 (the tier and the expired batch)", n, err)
	}
	if r, err := scanner.ScanReorder(t.Context(), tid); err != nil || r != 1 {
		t.Fatalf("reorder scan raised %d, err %v; want 1", r, err)
	}

	has := func(category, titlePart string) bool {
		for _, it := range inbox(t, f, tok) {
			if it.Category == category && strings.Contains(it.Title, titlePart) {
				return true
			}
		}
		return false
	}
	waitFor(t, "the expiry alert in the administrator inbox", func() bool { return has("stock_expiring", "ALERT-1 batch SOON") })
	waitFor(t, "the expired-on-hand alert", func() bool { return has("stock_expiring", "Expired stock to set apart: ALERT-1 batch OLD, 5 units") })
	waitFor(t, "the reorder alert", func() bool { return has("stock_below_reorder", "Below reorder level: ALERT-1") })
	for _, it := range inbox(t, f, tok) {
		if strings.Contains(it.Title, "SOON") && (!strings.Contains(it.DeepLink, "/inventory/batches/") || !strings.Contains(it.Body, "last day to sell")) {
			t.Fatalf("the alert must say when to stop selling and link to the batch: %+v", it)
		}
	}

	// No daily repeat: scanning again inside the same tier and day raises nothing.
	if n, err := scanner.ScanExpiry(t.Context(), tid); err != nil || n != 0 {
		t.Fatalf("a second scan raised %d alerts, err %v; want 0", n, err)
	}
	if n, err := scanner.ScanReorder(t.Context(), tid); err != nil || n != 0 {
		t.Fatalf("a second reorder scan raised %d, err %v; want 0", n, err)
	}
}
