package query

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// FefoBatchesQuery — return all live, in-stock, not-yet-expired
// batches for the given product, ordered (expiry_date ASC, id ASC)
// per BRD §6.5 First-Expired-First-Out canon.
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer). No cursor pagination — the dispatch picker needs
// the full set to compute multi-batch allocations.
type FefoBatchesQuery struct {
	TenantID  tenant.ID
	ProductID product.ID
}

// FefoBatchesHandler returns the FEFO-ordered batch list.
type FefoBatchesHandler struct {
	batches batch.Repository
	window  sellability.Resolver
}

// NewFefoBatchesHandler wires the handler. window resolves the company expiry
// rule (the business day and the one SQL bound) at the injected clock.
func NewFefoBatchesHandler(batches batch.Repository, window sellability.Resolver) FefoBatchesHandler {
	return FefoBatchesHandler{batches: batches, window: window}
}

// Handle returns the FEFO-ordered batches for the product projected to
// read-model Views (strict CQRS — the aggregate must not reach the port).
// Repository errors propagate; an empty result is `nil, nil` — the HTTP
// layer normalises to `[]` in the JSON wire shape.
func (h FefoBatchesHandler) Handle(ctx context.Context, q FefoBatchesQuery) ([]BatchView, error) {
	w, err := h.window.Resolve(ctx, q.TenantID)
	if err != nil {
		return nil, fmt.Errorf("fefo batches: %w", err)
	}
	out, err := h.batches.ListFefoForProduct(ctx, q.TenantID, q.ProductID, w.MinSellable)
	if err != nil {
		return nil, fmt.Errorf("fefo batches: %w", err)
	}
	views := make([]BatchView, 0, len(out))
	for _, b := range out {
		views = append(views, projectBatch(b, w))
	}
	return views, nil
}
