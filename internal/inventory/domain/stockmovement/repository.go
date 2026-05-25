package stockmovement

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
)

// ErrNotFound — no movement matched the supplied ID in the caller's
// tenant scope. (Movements aren't soft-deleted; rows are append-only.)
var ErrNotFound = errs.New(errs.KindNotFound, "stock_movement", "stock movement not found")

// Repository persists Movement aggregates. Append-only — no Update path.
//
// The Movement INSERT runs on the SAME tx as the parent Batch UPDATE
// per ADR 0008 + the StockMovement command handler's UoW boundary.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an ID
// without an aggregate ALSO takes an EXPLICIT tenantID parameter. The
// adapter binds the GUC from the parameter at tx-begin (NOT from ctx-
// tenancy.WithID — that's a domain value in context, which Khorikov §11
// + Cheney mark as a hidden input). RLS remains the security backstop.
type Repository interface {
	// Add appends a new ledger row. The aggregate already carries its
	// TenantID — no separate param needed. Drains the aggregate's
	// events into the outbox same-tx. Joins the surrounding UoW tx via
	// pg.TxFromContext when present.
	Add(ctx context.Context, m *Movement) error

	// GetByID returns a movement by ID in the supplied tenant scope or
	// ErrNotFound.
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Movement, error)

	// ListByBatchPage returns the per-batch ledger in the supplied
	// tenant, keyset-paginated by (occurred_at DESC, id DESC) per ADR 0038.
	ListByBatchPage(ctx context.Context, tenantID tenant.ID, batchID batch.ID, req PageRequest) (pagination.Page[*Movement], error)
}
