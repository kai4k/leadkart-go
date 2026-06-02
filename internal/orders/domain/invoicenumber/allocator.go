package invoicenumber

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Allocator returns the next gapless number for a (tenant, fy, kind) triple.
// The pgx adapter runs UPDATE … RETURNING last_used on
// orders.invoice_number_sequences inside the UoW tx from ctx; rollback rolls
// back the increment (ADR 0063 §3). The row is created lazily via
// INSERT … ON CONFLICT DO NOTHING before the UPDATE.
//
// Callers MUST be inside a uow.WithinTx closure; calling outside a tx returns
// an error.
type Allocator interface {
	// Allocate returns the freshly-allocated number. ctx must carry a UoW tx
	// (pg.TxFromContext); calling outside a tx returns an error.
	Allocate(ctx context.Context, tenantID tenant.ID, fy FinancialYear, kind Kind) (Number, error)
}
