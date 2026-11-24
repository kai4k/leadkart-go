// Package ports holds Inventory inbound concrete implementations —
// HTTP handlers + future event subscribers. Mirror of the identity
// ports layout per TDL canon. DTOs translate external request/response
// shapes ↔ Application command + query types.
package ports

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/common/lifecycle"
)

// ----- Product ---------------------------------------------------------------

// ProductDto is the wire shape for a single Product. snake_case
// fields per the rest of the LeadKart API.
//
// Manufacturer uses `omitzero` (Go 1.24+ encoding/json tag) — sends the
// field when set, omits when empty. `omitempty` would also omit the
// JSON string `""` even if the domain explicitly set it; `omitzero`
// matches the domain's "empty manufacturer = absent" semantic without
// the cross-talk per ADR 0061 amendment 1 (M5).
type ProductDto struct {
	ID           string `json:"id"`
	TenantID     string `json:"tenant_id"`
	SKU          string `json:"sku"`
	Name         string `json:"name"`
	DosageForm   string `json:"dosage_form"`
	PackSize     string `json:"pack_size"`
	HSNCode      string `json:"hsn_code"`
	GSTRateBps   int    `json:"gst_rate_bps"`
	Manufacturer string `json:"manufacturer,omitzero"`
	IsActive     bool   `json:"is_active"`
	// GSTRateID and GSTRateLabel name the entry of the company GST list the product
	// uses (empty for a product that predates the list, or when the entry is gone).
	GSTRateID            string `json:"gst_rate_id,omitzero"`
	GSTRateLabel         string `json:"gst_rate_label,omitzero"`
	DefaultSaleRatePaise int64  `json:"default_sale_rate_paise"`
	GenericName          string `json:"generic_name,omitzero"`
	Composition          string `json:"composition,omitzero"`
	StorageCondition     string `json:"storage_condition,omitzero"`
	// DrugSchedule is the class id (absent when the product carries none);
	// DrugScheduleLabel is its current label in the company list.
	DrugSchedule      string    `json:"drug_schedule,omitzero"`
	DrugScheduleLabel string    `json:"drug_schedule_label,omitzero"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	// ProductCategory is never empty: a product stored without one reads "General".
	ProductCategory string `json:"product_category"`
	// ReorderLevel 0 means the low-stock alert is off for this product.
	ReorderLevel int `json:"reorder_level"`
	// ExpiryAlertThresholdDays 0 means the expiry alert is off for this product.
	ExpiryAlertThresholdDays int `json:"expiry_alert_threshold_days"`
	// The five stock buckets over the product's live batches. They sum:
	// stock_on_hand = stock_reserved + stock_available + stock_unsellable +
	// stock_quarantined. stock_available is the free units of sellable batches
	// (what can be sold); stock_unsellable the free units past the company
	// sellable cut-off (not quarantined yet); stock_quarantined the units set
	// apart as "Not for sale".
	StockOnHand      int64 `json:"stock_on_hand"`
	StockReserved    int64 `json:"stock_reserved"`
	StockAvailable   int64 `json:"stock_available"`
	StockUnsellable  int64 `json:"stock_unsellable"`
	StockQuarantined int64 `json:"stock_quarantined"`
	// EarliestExpiry is the soonest expiry among the sellable batches that
	// hold stock; left out when there is none.
	EarliestExpiry *time.Time `json:"earliest_expiry,omitzero"`
}

// CreateProductRequest — POST /api/v1/inventory/products.
type CreateProductRequest struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	DosageForm string `json:"dosage_form"`
	PackSize   string `json:"pack_size"`
	HSNCode    string `json:"hsn_code"`
	// GSTRateID is REQUIRED: the entry of the company GST rate list (GET
	// /company/gst-rates) the product uses. There is no default rate and no
	// category fallback; the product takes that entry's rate.
	GSTRateID    string `json:"gst_rate_id"`
	Manufacturer string `json:"manufacturer,omitzero"`
	// DefaultSaleRatePaise is the rate offered on a quotation line (absent or 0: none).
	DefaultSaleRatePaise *int64 `json:"default_sale_rate_paise,omitzero"`
	// GenericName, Composition and StorageCondition are optional free text.
	GenericName      *string `json:"generic_name,omitzero"`
	Composition      *string `json:"composition,omitzero"`
	StorageCondition *string `json:"storage_condition,omitzero"`
	// DrugSchedule is the id of a class of the company own list (GET
	// /company/drug-schedule-classes). Nullable unless the company chose mode
	// `required`; refused under mode `off`.
	DrugSchedule *string `json:"drug_schedule,omitzero"`
	// ProductCategory is optional: absent or blank is stored as "General".
	ProductCategory *string `json:"product_category,omitzero"`
	// ReorderLevel is optional: absent is 0 (no low-stock alert).
	ReorderLevel *int `json:"reorder_level,omitzero"`
	// ExpiryAlertThresholdDays is optional: absent is 90 (BRD §6.5); an
	// explicit 0 switches the expiry alert off and is stored as 0.
	ExpiryAlertThresholdDays *int `json:"expiry_alert_threshold_days,omitzero"`
}

// CreateProductResponse — 201 body.
type CreateProductResponse struct {
	ProductID string `json:"product_id"`
}

// UpdateProductRequest — PATCH /api/v1/inventory/products/{productId}.
// Sparse partial-update payload: only OMITTED fields are skipped; all
// supplied fields (including explicit null in JSON) are applied. Slice 1
// uses Go's nilable-pointer decode which CANNOT distinguish JSON null
// from a missing field — both arrive as `nil` here. Per ADR 0061
// amendment 1, this is documented as "supplied-key partial update"
// rather than true RFC 7396 merge-patch (which requires null = clear).
//
// Practical impact: callers who want to CLEAR `manufacturer` send the
// empty string `""`, not JSON null. Callers who want to LEAVE the field
// unchanged omit it entirely. Slice 2 may add a `json.RawMessage`-based
// decoder if a true null-clears semantic becomes required.
type UpdateProductRequest struct {
	Name         *string `json:"name,omitzero"`
	GSTRateID    *string `json:"gst_rate_id,omitzero"`
	Manufacturer *string `json:"manufacturer,omitzero"`
	// DefaultSaleRatePaise sets the rate offered on a quotation line (0 clears it).
	DefaultSaleRatePaise *int64  `json:"default_sale_rate_paise,omitzero"`
	GenericName          *string `json:"generic_name,omitzero"`
	Composition          *string `json:"composition,omitzero"`
	StorageCondition     *string `json:"storage_condition,omitzero"`
	// DrugSchedule sets the class id; "" clears it.
	DrugSchedule *string `json:"drug_schedule,omitzero"`
	// ProductCategory: a blank value stores the default category "General".
	ProductCategory          *string `json:"product_category,omitzero"`
	ReorderLevel             *int    `json:"reorder_level,omitzero"`
	ExpiryAlertThresholdDays *int    `json:"expiry_alert_threshold_days,omitzero"`
}

// ProductCategoryDto is one allowed product category. It carries no GST rate: a
// product takes its rate from the company own GST list.
type ProductCategoryDto struct {
	Category string `json:"category"`
}

// ListProductCategoriesResponse — GET /api/v1/inventory/product-categories.
// The whole list, in category-name order; it is reference data, not paginated.
type ListProductCategoriesResponse struct {
	Items []ProductCategoryDto `json:"items"`
}

// ListProductsResponse — GET /api/v1/inventory/products page.
type ListProductsResponse struct {
	Items      []ProductDto `json:"items"`
	HasMore    bool         `json:"has_more"`
	NextCursor string       `json:"next_cursor,omitzero"`
	// StatusCounts is on the first page only (no cursor): products per status, for the
	// status chips, whatever ?status= is.
	StatusCounts *lifecycle.Counts `json:"status_counts,omitzero"`
}

// ----- Batch -----------------------------------------------------------------

// BatchDto is the wire shape for a single Batch.
type BatchDto struct {
	ID              string    `json:"id"`
	ProductID       string    `json:"product_id"`
	TenantID        string    `json:"tenant_id"`
	BatchNumber     string    `json:"batch_number"`
	ManufactureDate time.Time `json:"manufacture_date"`
	// ExpiryMonth is the printed month ("2027-03"); ExpiryDate is its last day.
	ExpiryMonth                string    `json:"expiry_month"`
	ExpiryDate                 time.Time `json:"expiry_date"`
	ManufacturerName           string    `json:"manufacturer_name"`
	ManufacturingLicenceNumber string    `json:"manufacturing_licence_number"`
	MRPPaise                   int64     `json:"mrp_paise"`
	PurchasePricePaise         int64     `json:"purchase_price_paise"`
	QuantityOnHand             int64     `json:"quantity_on_hand"`
	// QuantityReserved is the part of QuantityOnHand held for confirmed
	// orders; only the free units can be sold, moved out or adjusted down.
	QuantityReserved int64 `json:"quantity_reserved"`
	// QuantityQuarantined is the part of QuantityOnHand set apart as "Not for sale".
	QuantityQuarantined int64 `json:"quantity_quarantined"`
	// QuantityAvailable and QuantityUnsellable split the free units by whether
	// the batch is still sellable under the company expiry rule today.
	QuantityAvailable  int64 `json:"quantity_available"`
	QuantityUnsellable int64 `json:"quantity_unsellable"`
	// Sellable is the one expiry rule applied to this batch today;
	// LastSellableDay (YYYY-MM-DD) is the last day it may be sold;
	// PastPrintedExpiry is the legal limit (no setting moves it).
	Sellable          bool      `json:"sellable"`
	LastSellableDay   string    `json:"last_sellable_day"`
	PastPrintedExpiry bool      `json:"past_printed_expiry"`
	Version           int64     `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// AddBatchRequest — POST /api/v1/inventory/products/{productId}/batches.
type AddBatchRequest struct {
	BatchNumber     string    `json:"batch_number"`
	ManufactureDate time.Time `json:"manufacture_date"`
	// ExpiryMonth is the month printed on the label ("2027-03"); the batch is
	// valid through its last day. A full date is not accepted.
	ExpiryMonth                string `json:"expiry_month"`
	ManufacturerName           string `json:"manufacturer_name"`
	ManufacturingLicenceNumber string `json:"manufacturing_licence_number"`
	MRPPaise                   int64  `json:"mrp_paise"`
	PurchasePricePaise         int64  `json:"purchase_price_paise"`
	// InitialQuantity is optional: the opening stock received with the batch,
	// written as an inbound movement in the same transaction. Absent or 0
	// adds an empty batch, as before.
	InitialQuantity *int64 `json:"initial_quantity,omitzero"`
}

// AddBatchResponse — 201 body.
type AddBatchResponse struct {
	BatchID string `json:"batch_id"`
	// Warnings names non-fatal conditions, for example "past_sellable_cutoff":
	// opening stock was received into a batch already past the company cut-off.
	Warnings []string `json:"warnings"`
}

// ListBatchesResponse — GET /api/v1/inventory/products/{productId}/batches page.
type ListBatchesResponse struct {
	Items      []BatchDto `json:"items"`
	HasMore    bool       `json:"has_more"`
	NextCursor string     `json:"next_cursor,omitzero"`
}

// FefoBatchesResponse — GET /api/v1/inventory/products/{productId}/fefo-batches.
// No pagination: dispatch pickers need the FULL eligible set to compute
// multi-batch allocations against a single order quantity (BRD §6.5).
type FefoBatchesResponse struct {
	Items []BatchDto `json:"items"`
}

// ----- StockMovement ---------------------------------------------------------

// MovementDto is the wire shape for a single ledger row.
//
// SourceReference is `*string` (not `string` + `omitempty`) so the
// round-trip preserves the distinction between "absent" (nil) and
// "explicit empty" (a non-nil empty string) — required by the domain
// which stores it as `*string` per ADR 0061. Empty-string semantics
// differ from absent-pointer semantics on the JSON wire.
type MovementDto struct {
	ID                  string    `json:"id"`
	BatchID             string    `json:"batch_id"`
	ProductID           string    `json:"product_id"`
	TenantID            string    `json:"tenant_id"`
	Type                string    `json:"type"`
	Quantity            int64     `json:"quantity"`
	QuantityOnHandAfter int64     `json:"quantity_on_hand_after"`
	Reason              string    `json:"reason"`
	ActorMembershipID   string    `json:"actor_membership_id"`
	SourceReference     *string   `json:"source_reference,omitzero"`
	OccurredAt          time.Time `json:"occurred_at"`
}

// LogMovementRequest — POST /api/v1/inventory/batches/{batchId}/movements.
//
// Quantity is the POSITIVE magnitude. Direction is implicit in Type
// (Outbound subtracts; Adjustment up/down requires a future
// `direction` field — slice 1 stores positive magnitude for Adjustment).
type LogMovementRequest struct {
	Type            string  `json:"type"`
	Quantity        int64   `json:"quantity"`
	Reason          string  `json:"reason"`
	SourceReference *string `json:"source_reference,omitzero"`
}

// LogMovementResponse — 201 body. Returns the new MovementID + the
// batch's post-mutation on-hand for SPA reconciliation.
type LogMovementResponse struct {
	MovementID               string `json:"movement_id"`
	QuantityOnHandAfter      int64  `json:"quantity_on_hand_after"`
	QuantityQuarantinedAfter int64  `json:"quantity_quarantined_after"`
	// Warnings names non-fatal conditions, for example "past_sellable_cutoff".
	Warnings []string `json:"warnings"`
}

// ListMovementsResponse — GET /api/v1/inventory/batches/{batchId}/movements.
type ListMovementsResponse struct {
	Items      []MovementDto `json:"items"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor,omitzero"`
}

// ----- Error envelope --------------------------------------------------------

// ErrorResponse mirrors the identity port's RFC 9457 Problem Details
// shape with LeadKart legacy fields preserved. Keeping the shape
// per-module decouples wire-shape iterations between bounded contexts.
type ErrorResponse struct {
	// RFC 9457 fields
	Type   string `json:"type,omitzero"`
	Title  string `json:"title,omitzero"`
	Status int    `json:"status,omitzero"`
	Detail string `json:"detail,omitzero"`
	// LeadKart legacy fields
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}

// Error codes — snake_case enum surfaced in the legacy `error` field.
const (
	ErrCodeInvalidBody         = "invalid_body"
	ErrCodeInvalidQuery        = "invalid_query"
	ErrCodeInvalidID           = "invalid_id"
	ErrCodeInvalidPayload      = "invalid_payload"
	ErrCodeValidationFailed    = "validation_failed"
	ErrCodeInvalidMovementType = "invalid_movement_type"
	ErrCodeNotFound            = "not_found"
	ErrCodeConflict            = "conflict"
	ErrCodeAlreadyExists       = "already_exists"
	ErrCodeSKUTakenByInactive  = "product_sku_taken_by_inactive"
	ErrCodeUnauthenticated     = "unauthenticated"
	ErrCodeForbidden           = "forbidden"
	ErrCodeInsufficientStock   = "insufficient_stock"
	// ErrCodeInsufficientQuarantined: a release or write-off asked for more
	// quarantined units than the batch holds.
	ErrCodeInsufficientQuarantined = "insufficient_quarantined"
	ErrCodeBatchExpired            = "batch_expired"
	ErrCodeProductInactive         = "product_inactive"
	ErrCodeInvalidStatusFilter     = "invalid_status_filter"
	ErrCodeConcurrencyConflict     = "concurrency_conflict"
	ErrCodeInternalError           = "internal_error"
)
