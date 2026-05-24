// Package ports holds Inventory inbound concrete implementations —
// HTTP handlers + future event subscribers. Mirror of the identity
// ports layout per TDL canon. DTOs translate external request/response
// shapes ↔ Application command + query types.
package ports

import (
	"time"
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
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	SKU          string    `json:"sku"`
	Name         string    `json:"name"`
	DosageForm   string    `json:"dosage_form"`
	PackSize     string    `json:"pack_size"`
	HSNCode      string    `json:"hsn_code"`
	GSTRateBps   int       `json:"gst_rate_bps"`
	Manufacturer string    `json:"manufacturer,omitzero"`
	IsActive     bool      `json:"is_active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CreateProductRequest — POST /api/v1/inventory/products.
type CreateProductRequest struct {
	SKU          string `json:"sku"`
	Name         string `json:"name"`
	DosageForm   string `json:"dosage_form"`
	PackSize     string `json:"pack_size"`
	HSNCode      string `json:"hsn_code"`
	GSTRateBps   int    `json:"gst_rate_bps"`
	Manufacturer string `json:"manufacturer,omitzero"`
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
	GSTRateBps   *int    `json:"gst_rate_bps,omitzero"`
	IsActive     *bool   `json:"is_active,omitzero"`
	Manufacturer *string `json:"manufacturer,omitzero"`
}

// ListProductsResponse — GET /api/v1/inventory/products page.
type ListProductsResponse struct {
	Items      []ProductDto `json:"items"`
	HasMore    bool         `json:"has_more"`
	NextCursor string       `json:"next_cursor,omitzero"`
}

// ----- Batch -----------------------------------------------------------------

// BatchDto is the wire shape for a single Batch.
type BatchDto struct {
	ID                         string    `json:"id"`
	ProductID                  string    `json:"product_id"`
	TenantID                   string    `json:"tenant_id"`
	BatchNumber                string    `json:"batch_number"`
	ManufactureDate            time.Time `json:"manufacture_date"`
	ExpiryDate                 time.Time `json:"expiry_date"`
	ManufacturerName           string    `json:"manufacturer_name"`
	ManufacturingLicenceNumber string    `json:"manufacturing_licence_number"`
	MRPPaise                   int64     `json:"mrp_paise"`
	PurchasePricePaise         int64     `json:"purchase_price_paise"`
	QuantityOnHand             int64     `json:"quantity_on_hand"`
	Version                    int64     `json:"version"`
	CreatedAt                  time.Time `json:"created_at"`
	UpdatedAt                  time.Time `json:"updated_at"`
}

// AddBatchRequest — POST /api/v1/inventory/products/{productId}/batches.
type AddBatchRequest struct {
	BatchNumber                string    `json:"batch_number"`
	ManufactureDate            time.Time `json:"manufacture_date"`
	ExpiryDate                 time.Time `json:"expiry_date"`
	ManufacturerName           string    `json:"manufacturer_name"`
	ManufacturingLicenceNumber string    `json:"manufacturing_licence_number"`
	MRPPaise                   int64     `json:"mrp_paise"`
	PurchasePricePaise         int64     `json:"purchase_price_paise"`
}

// AddBatchResponse — 201 body.
type AddBatchResponse struct {
	BatchID string `json:"batch_id"`
}

// ListBatchesResponse — GET /api/v1/inventory/products/{productId}/batches page.
type ListBatchesResponse struct {
	Items      []BatchDto `json:"items"`
	HasMore    bool       `json:"has_more"`
	NextCursor string     `json:"next_cursor,omitzero"`
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
	MovementID          string `json:"movement_id"`
	QuantityOnHandAfter int64  `json:"quantity_on_hand_after"`
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
	Error   string              `json:"error,omitzero"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}

// Error codes — snake_case enum surfaced in the legacy `error` field.
const (
	ErrCodeInvalidBody       = "invalid_body"
	ErrCodeInvalidQuery      = "invalid_query"
	ErrCodeInvalidID         = "invalid_id"
	ErrCodeInvalidPayload    = "invalid_payload"
	ErrCodeValidationFailed  = "validation_failed"
	ErrCodeNotFound          = "not_found"
	ErrCodeConflict          = "conflict"
	ErrCodeAlreadyExists     = "already_exists"
	ErrCodeUnauthenticated   = "unauthenticated"
	ErrCodeForbidden         = "forbidden"
	ErrCodeInsufficientStock = "insufficient_stock"
	ErrCodeBatchExpired      = "batch_expired"
	ErrCodeProductHasStock   = "product_has_live_stock"
	ErrCodeConcurrencyConflict = "concurrency_conflict"
	ErrCodeInternalError     = "internal_error"
)
