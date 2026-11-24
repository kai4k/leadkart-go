//go:build integration

// arch-test:no-timeout-needed — shared pgtest container; each case is a few short tenant-scoped statements.
// arch-test:parallel-safe — fresh tenant ids, RLS isolation.
// arch-test:raw-sql-justified — the two CHECK tests must write rows the domain can no longer produce (a batch whose held units exceed on hand, an expiry that is not a month end); only a raw UPDATE reaches the constraint.

package adapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// Review G2 mutant: the sellable bound in the reorder scan. Units of an expired batch
// do not count as stock: a product holding only expired units is below its level.
func TestAlertScan_ReorderCountsOnlySellableStock(t *testing.T) {
	t.Parallel()
	rig := newStockRig(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	w := windowAt(now, 0)

	onlyExpired := rig.product(t, product.Spec{DosageForm: "Tablet", ReorderLevel: 10})
	rig.batch(t, onlyExpired, monthEnd(2026, time.May), 50, 0, 0) // May is over on 15 June
	healthy := rig.product(t, product.Spec{DosageForm: "Tablet", ReorderLevel: 10})
	rig.batch(t, healthy, monthEnd(2027, time.June), 50, 0, 0)

	scan := adapters.NewAlertScanRepository(repoFixture(t), pg.NewTransactor(repoFixture(t)))
	got, err := scan.ListProductsBelowReorder(t.Context(), uuid.MustParse(rig.tid.String()), w.MinSellable)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the product whose units are all expired is below its level")
	require.Equal(t, string(onlyExpired), got[0].ProductID.String())
	require.Zero(t, got[0].StockOnHand)
}

// Review G2 mutant: held-units CHECK. The database itself refuses reserved + quarantined above on hand.
func TestBatchesTable_RefusesMoreHeldUnitsThanOnHand(t *testing.T) {
	t.Parallel()
	rig := newStockRig(t)
	p := rig.product(t, product.Spec{DosageForm: "Tablet"})
	rig.batch(t, p, monthEnd(2027, time.June), 10, 6, 0)
	err := rig.rawExec(t, `UPDATE inventory.batches SET quantity_quarantined = 5 WHERE product_id = $1`, string(p))
	require.ErrorContains(t, err, "chk_batches_held_le_onhand")
}

// Review G2 mutant: month-end CHECK. The database itself refuses an expiry that is not a month end.
func TestBatchesTable_RefusesAnExpiryThatIsNotAMonthEnd(t *testing.T) {
	t.Parallel()
	rig := newStockRig(t)
	p := rig.product(t, product.Spec{DosageForm: "Tablet"})
	rig.batch(t, p, monthEnd(2027, time.June), 10, 0, 0)
	err := rig.rawExec(t, `UPDATE inventory.batches SET expiry_date = DATE '2027-06-15' WHERE product_id = $1`, string(p))
	require.ErrorContains(t, err, "chk_batches_expiry_is_month_end")
}

// rawExec runs one statement under the rig tenant scope (RLS applies as on the request path).
func (r *stockRig) rawExec(t *testing.T, sql string, args ...any) error {
	t.Helper()
	tx, err := repoFixture(t).Begin(r.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.ctx)) }()
	_, err = tx.Exec(r.ctx, `SELECT set_config('app.tenant_id', $1, true)`, r.tid.String())
	require.NoError(t, err)
	_, err = tx.Exec(r.ctx, sql, args...)
	return err
}

// ADR 0073 (review G2j M1): an INACTIVE product raises no reorder alert (its stock may still expire,
// so the expiry scan keeps it, but nobody reorders what is no longer sold). Mutant: the reorder
// query's `p.is_active` filter replaced by true.
func TestAlertScan_ReorderSkipsAnInactiveProduct(t *testing.T) {
	t.Parallel()
	rig := newStockRig(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	w := windowAt(now, 0)
	active := rig.product(t, product.Spec{DosageForm: "Tablet", ReorderLevel: 10})
	inactive := rig.product(t, product.Spec{DosageForm: "Tablet", ReorderLevel: 10})
	require.NoError(t, rig.products.UpdateByID(rig.ctx, rig.tid, inactive, func(p *product.Product) (bool, error) {
		return true, p.Deactivate(rig.actor, now)
	}))

	scan := adapters.NewAlertScanRepository(repoFixture(t), pg.NewTransactor(repoFixture(t)))
	got, err := scan.ListProductsBelowReorder(t.Context(), uuid.MustParse(rig.tid.String()), w.MinSellable)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the active product below its level is alerted")
	require.Equal(t, string(active), got[0].ProductID.String())
}
