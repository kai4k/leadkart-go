package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// MovementView is the flat read model for a single stock movement. Per
// STRICT CQRS (TDL canon) query handlers project the write aggregate
// into this read DTO; the [stockmovement.Movement] aggregate NEVER
// leaks past the app layer into ports/. The port serializes this View
// into the wire MovementDto (1:1).
type MovementView struct {
	ID                  string
	BatchID             string
	ProductID           string
	TenantID            string
	Type                string
	Quantity            int64
	QuantityOnHandAfter int64
	Reason              string
	ActorMembershipID   string
	SourceReference     *string
	OccurredAt          time.Time
}

// projectMovement maps the write aggregate to the flat read View — the
// single source of truth for movement read projection.
func projectMovement(m *stockmovement.Movement) MovementView {
	return MovementView{
		ID:                  m.ID().String(),
		BatchID:             m.BatchID().String(),
		ProductID:           m.ProductID().String(),
		TenantID:            m.TenantID().String(),
		Type:                string(m.Type()),
		Quantity:            m.Quantity(),
		QuantityOnHandAfter: m.QuantityOnHandAfter(),
		Reason:              m.Reason(),
		ActorMembershipID:   m.ActorMembershipID().String(),
		// *string round-trip preserves absent-vs-empty distinction per
		// ADR 0061 amendment 1 (M3) — domain stores *string.
		SourceReference: m.SourceReference(),
		OccurredAt:      m.OccurredAt(),
	}
}

// ListBatchMovementsPageQuery — cursor-paginated per-batch ledger read.
// Optionally filtered by movement Type per the route's ?type=&cursor=
// query string.
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer). Per ADR 0062 (TDL canon): tenantID flows through
// explicit query fields, not via context smuggling.
type ListBatchMovementsPageQuery struct {
	TenantID tenant.ID
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

// Handle returns the page of MovementView.
func (h ListBatchMovementsPageHandler) Handle(ctx context.Context, q ListBatchMovementsPageQuery) (pagination.Page[MovementView], error) {
	req := stockmovement.PageRequest{
		Filter:   stockmovement.ListFilter{Type: q.Type},
		Cursor:   q.Cursor,
		PageSize: pagination.ClampPageSize(q.PageSize),
	}
	if !req.Filter.IsValid() {
		return pagination.Page[MovementView]{}, fmt.Errorf("%w: type=%q", stockmovement.ErrInvalid, q.Type)
	}
	page, err := h.movements.ListByBatchPage(ctx, q.TenantID, q.BatchID, req)
	if err != nil {
		return pagination.Page[MovementView]{}, fmt.Errorf("list movements: %w", err)
	}
	views := make([]MovementView, 0, len(page.Items))
	for _, m := range page.Items {
		views = append(views, projectMovement(m))
	}
	return pagination.Page[MovementView]{
		Items:      views,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}, nil
}
