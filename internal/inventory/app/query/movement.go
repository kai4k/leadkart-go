package query

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// ListBatchMovementsPageQuery — cursor-paginated per-batch ledger read.
// Optionally filtered by movement Type per the route's ?type=&cursor=
// query string.
type ListBatchMovementsPageQuery struct {
	BatchID  batch.ID
	Cursor   pagination.Cursor
	PageSize int
	Type     batch.MovementType
}

// ListBatchMovementsPageHandler returns a keyset-paginated page.
type ListBatchMovementsPageHandler struct {
	movements stockmovement.Repository
}

// NewListBatchMovementsPageHandler wires the handler.
func NewListBatchMovementsPageHandler(movements stockmovement.Repository) ListBatchMovementsPageHandler {
	return ListBatchMovementsPageHandler{movements: movements}
}

// Handle returns the page.
func (h ListBatchMovementsPageHandler) Handle(ctx context.Context, q ListBatchMovementsPageQuery) (pagination.Page[*stockmovement.Movement], error) {
	req := stockmovement.PageRequest{
		Filter:   stockmovement.ListFilter{Type: q.Type},
		Cursor:   q.Cursor,
		PageSize: pagination.ClampPageSize(q.PageSize),
	}
	if !req.Filter.IsValid() {
		return pagination.Page[*stockmovement.Movement]{}, fmt.Errorf("%w: type=%q", stockmovement.ErrInvalid, q.Type)
	}
	page, err := h.movements.ListByBatchPage(ctx, q.BatchID, req)
	if err != nil {
		return pagination.Page[*stockmovement.Movement]{}, fmt.Errorf("list movements: %w", err)
	}
	return page, nil
}
