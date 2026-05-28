package invoicenumber

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Allocator is the canonical "give me the next gapless number for this
// (tenant, fy, kind)" primitive. The pgx-backed impl in adapters
// performs `UPDATE … RETURNING last_used` on
// `orders.invoice_number_sequences` INSIDE the UoW tx supplied via
// ctx — so rollback rolls back the increment per ADR 0063 §3.
//
// First-call-per-(tenant, fy, kind) semantics: the adapter does
// `INSERT … ON CONFLICT DO NOTHING; UPDATE … RETURNING last_used` so
// the row is materialised lazily without races.
//
// The handler that wraps Allocator MUST be inside a uow.WithinTx
// closure — calling Allocator outside a tx breaks the gaplessness
// guarantee (the adapter signals this with an error rather than
// silently opening its own tx).
type Allocator interface {
	// Allocate returns the freshly-allocated number for the supplied
	// (tenant, fy, kind). Implementations MUST run inside a UoW tx
	// (pulled from ctx via [pg.TxFromContext]); calling outside a tx
	// returns an error.
	Allocate(ctx context.Context, tenantID tenant.ID, fy FinancialYear, kind Kind) (Number, error)
}
