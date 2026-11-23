package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// BatchExpiringSoonV1 — a batch with stock has crossed one expiry warning tier.
// Wire alias: `inventory.batch_expiring_soon.v1`. Tenant-scoped.
//
// DaysUntilExpiry counts to the LAST SELLABLE DAY (the company cut-off), not to
// the printed expiry; it is negative once that day has passed. TierDays is the
// warning tier this alert is for (0: the sellable window has closed). One alert
// is raised per tier crossed, never one per day.
type BatchExpiringSoonV1 struct {
	BatchID         uuid.UUID `json:"batch_id"`
	ProductID       uuid.UUID `json:"product_id"`
	TenantIDClaim   uuid.UUID `json:"tenant_id"`
	BatchNumber     string    `json:"batch_number"`
	ProductSKU      string    `json:"product_sku"`
	ProductName     string    `json:"product_name"`
	ExpiryDate      time.Time `json:"expiry_date"`
	LastSellableDay time.Time `json:"last_sellable_day"`
	DaysUntilExpiry int       `json:"days_until_expiry"`
	TierDays        int       `json:"tier_days"`
	// FreeQuantity is the units not reserved and not quarantined when the alert fired.
	FreeQuantity  int64     `json:"free_quantity"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (BatchExpiringSoonV1) Topic() string { return "inventory.batch_expiring_soon.v1" }

// OccurredAt returns the domain timestamp.
func (e BatchExpiringSoonV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e BatchExpiringSoonV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// BatchExpiredOnHandV1 — units past their PRINTED expiry are still on hand and
// not set apart. The law requires such stock to be stored apart and marked "Not
// for sale" (Drugs Rules, Rule 65(17)); the alert tells staff to quarantine it.
// Wire alias: `inventory.batch_expired_on_hand.v1`. Tenant-scoped. Raised once per
// batch.
type BatchExpiredOnHandV1 struct {
	BatchID       uuid.UUID `json:"batch_id"`
	ProductID     uuid.UUID `json:"product_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	BatchNumber   string    `json:"batch_number"`
	ProductSKU    string    `json:"product_sku"`
	ProductName   string    `json:"product_name"`
	ExpiryDate    time.Time `json:"expiry_date"`
	// Quantity is the on-hand units not yet quarantined.
	Quantity      int64     `json:"quantity"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (BatchExpiredOnHandV1) Topic() string { return "inventory.batch_expired_on_hand.v1" }

// OccurredAt returns the domain timestamp.
func (e BatchExpiredOnHandV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e BatchExpiredOnHandV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ProductBelowReorderLevelV1 — a product's sellable stock fell below its
// reorder level. Wire alias: `inventory.product_below_reorder_level.v1`.
// Tenant-scoped.
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

var (
	_ TenantScoped = BatchExpiringSoonV1{}
	_ TenantScoped = BatchExpiredOnHandV1{}
	_ TenantScoped = ProductBelowReorderLevelV1{}

	_ = register(BatchExpiringSoonV1{})
	_ = register(BatchExpiredOnHandV1{})
	_ = register(ProductBelowReorderLevelV1{})
)
