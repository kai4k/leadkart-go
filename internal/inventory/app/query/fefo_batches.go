package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
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
	now     func() time.Time
}

// NewFefoBatchesHandler wires the handler. `now` is the explicit time
// source — composition root wires `time.Now`; tests inject a fixed-time
// closure. Nil → time.Now.
func NewFefoBatchesHandler(batches batch.Repository, now func() time.Time) FefoBatchesHandler {
	if now == nil {
		now = time.Now
	}
	return FefoBatchesHandler{batches: batches, now: now}
}

// Handle returns the FEFO-ordered batches for the product projected to
// read-model Views (strict CQRS — the aggregate must not reach the port).
// Repository errors propagate; an empty result is `nil, nil` — the HTTP
// layer normalises to `[]` in the JSON wire shape.
func (h FefoBatchesHandler) Handle(ctx context.Context, q FefoBatchesQuery) ([]BatchView, error) {
	out, err := h.batches.ListFefoForProduct(ctx, q.TenantID, q.ProductID, h.now())
	if err != nil {
		return nil, fmt.Errorf("fefo batches: %w", err)
	}
	views := make([]BatchView, 0, len(out))
	for _, b := range out {
		views = append(views, projectBatch(b))
	}
	return views, nil
}
