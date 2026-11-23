package subscribers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryevents "github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
	"github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

// arch-test:idempotency-via-dedup-window — every notification routes through
// CreateNotification → repository.Add, whose time-windowed dedup probe suppresses a
// re-delivered alert (ErrDuplicateInDedupWindow → success); the router
// IdempotencyMiddleware is the primary dedup.

// Handler names for the stock alerts — CI-stable. DO NOT rename.
const (
	HandlerBatchExpiringSoon  = "notifications.subscribers.BatchExpiringSoon"
	HandlerBatchExpiredOnHand = "notifications.subscribers.BatchExpiredOnHand"
	HandlerBelowReorder       = "notifications.subscribers.ProductBelowReorderLevel"
)

// StockAlertSubscriber turns the Inventory stock alerts into inbox notifications,
// one per recipient. Before G2 none of these events had a consumer: expiry and
// reorder alerts reached nobody.
type StockAlertSubscriber struct {
	create     command.CreateNotificationHandler
	recipients command.StockAlertRecipientResolver
	log        *slog.Logger
}

// NewStockAlertSubscriber wires the subscriber. recipients and log are required.
func NewStockAlertSubscriber(create command.CreateNotificationHandler, recipients command.StockAlertRecipientResolver, log *slog.Logger) *StockAlertSubscriber {
	if recipients == nil || log == nil {
		panic("subscribers: NewStockAlertSubscriber recipients and log required")
	}
	return &StockAlertSubscriber{create: create, recipients: recipients, log: log}
}

// notify creates one notification per recipient. A company with no one to tell is
// logged and acknowledged: retrying cannot make a recipient appear.
func (h *StockAlertSubscriber) notify(ctx context.Context, tid tenant.ID, tpl command.CreateNotificationCommand) error {
	people, err := h.recipients.StockAlertRecipients(ctx, tid)
	if err != nil {
		return fmt.Errorf("notifications stock alert: recipients: %w", err)
	}
	if len(people) == 0 {
		h.log.WarnContext(ctx, "notifications: a stock alert has no recipient", "tenant_id", tid.String(), "entity", tpl.SourceEntityID)
		return nil
	}
	for _, m := range people {
		cmd := tpl
		cmd.TenantID, cmd.RecipientMembershipID = tid, m
		if _, err := h.create.Handle(ctx, cmd); err != nil {
			return fmt.Errorf("notifications stock alert: %w", err)
		}
	}
	return nil
}

const alertDateLayout = "2 Jan 2006"

// HandleExpiring is the typed cqrs handler for inventory.batch_expiring_soon.v1.
func (h *StockAlertSubscriber) HandleExpiring(ctx context.Context, evt *inventoryevents.BatchExpiringSoonV1) error {
	title := fmt.Sprintf("Expiry alert: %s batch %s has %d days left to sell", evt.ProductSKU, evt.BatchNumber, evt.DaysUntilExpiry)
	if evt.DaysUntilExpiry < 0 {
		title = fmt.Sprintf("%s batch %s is no longer for sale", evt.ProductSKU, evt.BatchNumber)
	}
	body := fmt.Sprintf("%s: last day to sell %s (printed expiry %s). %d units are free.",
		evt.ProductName, evt.LastSellableDay.UTC().Format(alertDateLayout), evt.ExpiryDate.UTC().Format(alertDateLayout), evt.FreeQuantity)
	return h.notify(ctx, tenant.ID(evt.TenantIDClaim.String()), command.CreateNotificationCommand{
		Category: notification.CategoryStockExpiring, Title: title, Body: body,
		SourceModule: "inventory", SourceEntityType: "batch_expiry_tier",
		SourceEntityID: fmt.Sprintf("%s:%d", evt.BatchID, evt.TierDays),
		DeepLink:       "/inventory/batches/" + evt.BatchID.String(),
	})
}

// HandleExpiredOnHand is the typed cqrs handler for inventory.batch_expired_on_hand.v1.
func (h *StockAlertSubscriber) HandleExpiredOnHand(ctx context.Context, evt *inventoryevents.BatchExpiredOnHandV1) error {
	return h.notify(ctx, tenant.ID(evt.TenantIDClaim.String()), command.CreateNotificationCommand{
		Category: notification.CategoryStockExpiring,
		Title:    fmt.Sprintf("Expired stock to set apart: %s batch %s, %d units", evt.ProductSKU, evt.BatchNumber, evt.Quantity),
		Body: fmt.Sprintf("%s expired on %s. Expired stock must be stored apart and marked \"Not for sale\": quarantine it, then return or write it off.",
			evt.ProductName, evt.ExpiryDate.UTC().Format(alertDateLayout)),
		SourceModule: "inventory", SourceEntityType: "batch_expired", SourceEntityID: evt.BatchID.String(),
		DeepLink: "/inventory/batches/" + evt.BatchID.String(),
	})
}

// HandleBelowReorder is the typed cqrs handler for inventory.product_below_reorder_level.v1.
func (h *StockAlertSubscriber) HandleBelowReorder(ctx context.Context, evt *inventoryevents.ProductBelowReorderLevelV1) error {
	return h.notify(ctx, tenant.ID(evt.TenantIDClaim.String()), command.CreateNotificationCommand{
		Category:     notification.CategoryStockBelowReorder,
		Title:        fmt.Sprintf("Below reorder level: %s (%d in stock, level %d)", evt.SKU, evt.CurrentStockOnHand, evt.ReorderLevel),
		Body:         "Sellable stock is below the reorder level set for this product.",
		SourceModule: "inventory", SourceEntityType: "product_reorder",
		SourceEntityID: fmt.Sprintf("%s:%s", evt.ProductID, evt.OccurredAtUTC.UTC().Format(time.DateOnly)),
		DeepLink:       "/inventory/products/" + evt.ProductID.String(),
	})
}
