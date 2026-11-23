//go:build integration

// arch-test:no-timeout-needed — shared pgtest container; each case is a handful
// of short tenant-scoped reads over a few seeded rows.
// arch-test:parallel-safe — fresh tenant ids, RLS isolation.

package adapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// stockRig seeds products and batches for one tenant through the write repos.
type stockRig struct {
	ctx      context.Context
	tid      tenant.ID
	actor    membership.ID
	products *adapters.ProductRepository
	batches  *adapters.BatchRepository
	seq      int
}

func newStockRig(t *testing.T) *stockRig {
	t.Helper()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	tx := pg.NewTransactor(pool)
	return &stockRig{
		ctx: tenantCtx(t, tid), tid: tid, actor: membership.ID(ids.NewV7().String()),
		products: adapters.NewProductRepository(pool, tx), batches: adapters.NewBatchRepository(pool, tx),
	}
}

// product stores a product; each one is created a minute after the previous,
// so the list order (newest first) is the reverse of the seeding order.
func (r *stockRig) product(t *testing.T, spec product.Spec) product.ID {
	t.Helper()
	r.seq++
	spec.SKU = "STK-" + ids.NewV7().String()[24:]
	spec.Name, spec.PackSize, spec.HSNCode, spec.GSTRateBps = "Stock Drug", "10", "3004", 1200
	p, err := product.New(product.ID(ids.NewV7().String()), r.tid, r.actor, spec, fixedNow.Add(time.Duration(r.seq)*time.Minute))
	require.NoError(t, err)
	require.NoError(t, r.products.Add(r.ctx, p))
	return p.ID()
}

// batch stores a batch expiring on expiry holding onHand units, reserved of them held.
func (r *stockRig) batch(t *testing.T, productID product.ID, expiry time.Time, onHand, reserved int64) {
	t.Helper()
	b, err := batch.New(batch.ID(ids.NewV7().String()), productID, r.tid, r.actor, batch.Spec{
		BatchNumber:      "LOT-" + ids.NewV7().String()[24:],
		ManufactureDate:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:       expiry,
		ManufacturerName: "Acme", ManufacturingLicenceNumber: "ML-1", MRPPaise: 25000, PurchasePricePaise: 18000,
	}, fixedNow)
	require.NoError(t, err)
	if onHand > 0 {
		// Adjustment, not inbound: it is the one way stock reaches a batch that is already expired.
		require.NoError(t, b.ApplyMovement(batch.MovementAdjustment, onHand, fixedNow))
	}
	require.NoError(t, r.batches.Add(r.ctx, b))
	if reserved > 0 {
		require.NoError(t, r.batches.UpdateByID(r.ctx, r.tid, b.ID(), func(b *batch.Batch) (bool, error) {
			return true, b.ApplyMovement(batch.MovementReservation, reserved, fixedNow)
		}))
	}
}

func viewIDs(views []query.ProductView) []string {
	out := make([]string, 0, len(views))
	for _, v := range views {
		out = append(out, v.ID)
	}
	return out
}

// TestProductReader_StockTotalsAndFilters pins the product read model: the
// totals count live, unexpired batches only; reserved stock is reported and
// subtracted; the filters use the same figures; another tenant sees nothing.
func TestProductReader_StockTotalsAndFilters(t *testing.T) {
	t.Parallel()
	rig := newStockRig(t)
	reader := adapters.NewProductReader(repoFixture(t), pg.NewTransactor(repoFixture(t)))
	today := fixedNow // 2026-05-24
	day := func(offset int) time.Time {
		return time.Date(2026, 5, 24+offset, 0, 0, 0, 0, time.UTC)
	}

	a := rig.product(t, product.Spec{DosageForm: "Tablet", Manufacturer: "Acme", ProductCategory: "Cardiac", ReorderLevel: 100, ExpiryAlertThresholdDays: 30})
	rig.batch(t, a, day(30), 40, 15) // the soonest unexpired stock
	rig.batch(t, a, day(400), 20, 0)
	rig.batch(t, a, day(0), 5, 0)  // expires today: expired (expiry_date > today is false)
	rig.batch(t, a, day(10), 0, 0) // sooner than the first, but holds no stock
	b := rig.product(t, product.Spec{DosageForm: "Syrup", Manufacturer: "Zed"})
	rig.batch(t, b, day(400), 500, 0)
	c := rig.product(t, product.Spec{DosageForm: "Tablet", ReorderLevel: 10})

	other := newStockRig(t)
	d := other.product(t, product.Spec{DosageForm: "Tablet", ProductCategory: "Cardiac", ReorderLevel: 100})
	other.batch(t, d, day(5), 999, 0)

	// Detail.
	got, err := reader.GetProduct(rig.ctx, rig.tid, a, today)
	require.NoError(t, err)
	require.Equal(t, int64(60), got.StockOnHand, "expired units are not stock")
	require.Equal(t, int64(15), got.StockReserved)
	require.Equal(t, int64(45), got.StockAvailable)
	require.NotNil(t, got.EarliestExpiry)
	require.True(t, got.EarliestExpiry.Equal(day(30)), "earliest expiry %v, want %v", got.EarliestExpiry, day(30))
	require.Equal(t, "Cardiac", got.ProductCategory)
	require.Equal(t, 100, got.ReorderLevel)
	require.Equal(t, 30, got.ExpiryAlertThresholdDays)

	empty, err := reader.GetProduct(rig.ctx, rig.tid, c, today)
	require.NoError(t, err)
	require.Zero(t, empty.StockOnHand)
	require.Zero(t, empty.StockAvailable)
	require.Nil(t, empty.EarliestExpiry)
	require.Equal(t, product.ProductCategoryDefault, empty.ProductCategory)

	// On the day the 30-day batch expires it stops being stock, and so does its hold.
	later, err := reader.GetProduct(rig.ctx, rig.tid, a, day(30))
	require.NoError(t, err)
	require.Equal(t, int64(20), later.StockOnHand)
	require.Zero(t, later.StockReserved, "a hold on an expired batch is not counted with unexpired stock")

	// Tenant isolation, both ways.
	_, err = reader.GetProduct(rig.ctx, rig.tid, d, today)
	require.ErrorIs(t, err, product.ErrNotFound)
	_, err = reader.GetProduct(other.ctx, other.tid, a, today)
	require.ErrorIs(t, err, product.ErrNotFound)

	list := func(f query.ProductListFilter) []string {
		t.Helper()
		views, lerr := reader.ListProducts(rig.ctx, rig.tid, f, today, pagination.Cursor{}, 50)
		require.NoError(t, lerr)
		return viewIDs(views)
	}
	days := func(n int) *int { return &n }
	as, bs, cs := a.String(), b.String(), c.String()

	require.Equal(t, []string{cs, bs, as}, list(query.ProductListFilter{}), "newest first; the other tenant's product is absent")
	require.Equal(t, []string{cs, as}, list(query.ProductListFilter{LowStock: true}), "B has no reorder level")
	require.Equal(t, []string{as}, list(query.ProductListFilter{ExpiringWithinDays: days(30)}), "the bound is inclusive")
	require.Empty(t, list(query.ProductListFilter{ExpiringWithinDays: days(29)}))
	require.Empty(t, list(query.ProductListFilter{ExpiringWithinDays: days(0)}), "a batch expiring today is already expired")
	require.Equal(t, []string{bs, as}, list(query.ProductListFilter{ExpiringWithinDays: days(400)}))
	require.Equal(t, []string{as}, list(query.ProductListFilter{Category: "cardiac"}), "case-insensitive")
	require.Equal(t, []string{cs, bs}, list(query.ProductListFilter{Category: "General"}))
	require.Equal(t, []string{cs}, list(query.ProductListFilter{Category: "General", LowStock: true}))
	require.Equal(t, []string{bs}, list(query.ProductListFilter{DosageForm: "syrup"}))
	require.Equal(t, []string{bs}, list(query.ProductListFilter{Manufacturer: "zed"}))

	// The list rows carry the same figures as the detail.
	views, err := reader.ListProducts(rig.ctx, rig.tid, query.ProductListFilter{Category: "Cardiac"}, today, pagination.Cursor{}, 50)
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, got, views[0])

	// Keyset: limit, then the rest after the cursor.
	first, err := reader.ListProducts(rig.ctx, rig.tid, query.ProductListFilter{}, today, pagination.Cursor{}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{cs, bs}, viewIDs(first))
	rest, err := reader.ListProducts(rig.ctx, rig.tid, query.ProductListFilter{}, today,
		pagination.Cursor{SortValue: first[1].CreatedAt, ID: first[1].ID}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{as}, viewIDs(rest))
}
