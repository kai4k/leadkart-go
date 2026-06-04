package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// BatchExpiringSoonV1 — the daily ExpiryScanJob detected that the given
// Batch's expiry_date falls within product.expiry_alert_threshold_days
// from `today` (UTC). Consumers: Notifications module (in-app + email
// alert per BRD §6.5).
//
// DaysUntilExpiry is computed at scan time = (expiry_date - today) in
// days; negative values are possible only if a previous scan was
// skipped (the worker still emits to keep the alert log truthful, but
// the value telegraphs lateness for ops dashboards).
type BatchExpiringSoonV1 struct {
	BatchID         uuid.UUID `json:"batch_id"`
	ProductID       uuid.UUID `json:"product_id"`
	TenantIDClaim   uuid.UUID `json:"tenant_id"`
	BatchNumber     string    `json:"batch_number"`
	ExpiryDate      time.Time `json:"expiry_date"`
	DaysUntilExpiry int       `json:"days_until_expiry"`
	OccurredAtUTC   time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (BatchExpiringSoonV1) Topic() string { return "inventory.batch_expiring_soon.v1" }

// OccurredAt returns the domain timestamp.
func (e BatchExpiringSoonV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e BatchExpiringSoonV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ProductBelowReorderLevelV1 — the daily ReorderScanJob detected that
// the given Product's total live-batch quantity_on_hand fell below its
// reorder_level. Consumers: Notifications module (admin alert per
// BRD §6.5); future PurchaseOrder draft suggestion.
type ProductBelowReorderLevelV1 struct {
	ProductID          uuid.UUID `json:"product_id"`
	TenantIDClaim      uuid.UUID `json:"tenant_id"`
	SKU                string    `json:"sku"`
	ReorderLevel       int       `json:"reorder_level"`
	CurrentStockOnHand int64     `json:"current_stock_on_hand"`
	OccurredAtUTC      time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ProductBelowReorderLevelV1) Topic() string { return "inventory.product_below_reorder_level.v1" }

// OccurredAt returns the domain timestamp.
func (e ProductBelowReorderLevelV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ProductBelowReorderLevelV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time + runtime registration.
var (
	_ TenantScoped = BatchExpiringSoonV1{}
	_ TenantScoped = ProductBelowReorderLevelV1{}

	_ = register(BatchExpiringSoonV1{})
	_ = register(ProductBelowReorderLevelV1{})
)
