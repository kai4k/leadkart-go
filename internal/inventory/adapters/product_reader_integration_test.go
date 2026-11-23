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

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
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

// batch stores a batch expiring on expiry (a month end) holding onHand units, of
// which reserved are held for orders and quarantined are set apart.
func (r *stockRig) batch(t *testing.T, productID product.ID, expiry time.Time, onHand, reserved, quarantined int64) {
	t.Helper()
	b, err := batch.New(batch.ID(ids.NewV7().String()), productID, r.tid, r.actor, batch.Spec{
		BatchNumber:      "LOT-" + ids.NewV7().String()[24:],
		ManufactureDate:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:       expiry,
		ManufacturerName: "Acme", ManufacturingLicenceNumber: "ML-1", MRPPaise: 25000, PurchasePricePaise: 18000,
	}, fixedNow)
	require.NoError(t, err)
	if onHand > 0 {
		// Adjustment, not inbound: it is the one way stock reaches a batch that is already past its expiry.
		require.NoError(t, b.ApplyMovement(batch.MovementAdjustment, onHand, fixedNow, fixedNow))
	}
	require.NoError(t, r.batches.Add(r.ctx, b))
	// Holds go through the repository update, as in production (the insert writes on-hand only).
	if reserved > 0 || quarantined > 0 {
		require.NoError(t, r.batches.UpdateByID(r.ctx, r.tid, b.ID(), func(b *batch.Batch) (bool, error) {
			if reserved > 0 {
				if err := b.ApplyMovement(batch.MovementReservation, reserved, fixedNow, fixedNow); err != nil {
					return false, err
				}
			}
			if quarantined > 0 {
				if err := b.ApplyMovement(batch.MovementQuarantine, quarantined, fixedNow, fixedNow); err != nil {
					return false, err
				}
			}
			return true, nil
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

func monthEnd(y int, m time.Month) time.Time { return expiry.MonthEnd(y, m) }

func windowAt(now time.Time, stopMonths int) sellability.Window {
	p := commercepolicy.Default()
	p.Expiry.StopMonthsBefore = stopMonths
	return sellability.FromPolicy(p, now)
}

// TestProductReader_StockBucketsAndFilters pins the product read model under the
// one expiry bound: the five buckets sum to on-hand; quarantined units are never
// available; units past the company cut-off are "unsellable", not stock for
// sale; the filters use the same figures; another tenant sees nothing.
func TestProductReader_StockBucketsAndFilters(t *testing.T) {
	t.Parallel()
	rig := newStockRig(t)
	reader := adapters.NewProductReader(repoFixture(t), pg.NewTransactor(repoFixture(t)))
	w := windowAt(fixedNow, 1) // business day 2026-05-24, stop at the start of the printed month

	a := rig.product(t, product.Spec{DosageForm: "Tablet", Manufacturer: "Acme", ProductCategory: "Cardiac", ReorderLevel: 100, ExpiryAlertThresholdDays: 30})
	rig.batch(t, a, monthEnd(2026, time.July), 40, 15, 0)    // sellable; last sellable day 30 Jun
	rig.batch(t, a, monthEnd(2027, time.June), 20, 0, 0)     // sellable
	rig.batch(t, a, monthEnd(2026, time.May), 5, 0, 0)       // printed month is this one: past the cut-off, unsellable
	rig.batch(t, a, monthEnd(2026, time.December), 10, 0, 4) // 4 set apart
	rig.batch(t, a, monthEnd(2026, time.June), 0, 0, 0)      // sooner, but holds no stock
	b := rig.product(t, product.Spec{DosageForm: "Syrup", Manufacturer: "Zed"})
	rig.batch(t, b, monthEnd(2027, time.June), 500, 0, 0)
	c := rig.product(t, product.Spec{DosageForm: "Tablet", ReorderLevel: 10})

	other := newStockRig(t)
	d := other.product(t, product.Spec{DosageForm: "Tablet", ProductCategory: "Cardiac", ReorderLevel: 100})
	other.batch(t, d, monthEnd(2026, time.August), 999, 0, 0)

	got, err := reader.GetProduct(rig.ctx, rig.tid, a, w)
	require.NoError(t, err)
	require.Equal(t, int64(75), got.StockOnHand, "every live unit, expired and quarantined included")
	require.Equal(t, int64(15), got.StockReserved)
	require.Equal(t, int64(51), got.StockAvailable, "free units of sellable batches: 25 + 20 + 6")
	require.Equal(t, int64(5), got.StockUnsellable)
	require.Equal(t, int64(4), got.StockQuarantined)
	require.Equal(t, got.StockOnHand, got.StockReserved+got.StockAvailable+got.StockUnsellable+got.StockQuarantined, "the buckets sum to on-hand")
	require.NotNil(t, got.EarliestExpiry)
	require.True(t, got.EarliestExpiry.Equal(monthEnd(2026, time.July)), "earliest sellable expiry %v", got.EarliestExpiry)
	require.Equal(t, "Cardiac", got.ProductCategory)
	require.Equal(t, 100, got.ReorderLevel)

	empty, err := reader.GetProduct(rig.ctx, rig.tid, c, w)
	require.NoError(t, err)
	require.Zero(t, empty.StockOnHand)
	require.Nil(t, empty.EarliestExpiry)
	require.Equal(t, product.ProductCategoryDefault, empty.ProductCategory)

	// On 1 July the 31 July batch is past the cut-off too (its printed month is next month).
	later, err := reader.GetProduct(rig.ctx, rig.tid, a, windowAt(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 1))
	require.NoError(t, err)
	require.Equal(t, int64(26), later.StockAvailable, "only the Dec 2026 (6) and Jun 2027 (20) batches are still for sale")
	require.Equal(t, int64(30), later.StockUnsellable, "the 31 Jul batch (25 free) and the May batch (5) are past the cut-off")

	// Tenant isolation, both ways.
	_, err = reader.GetProduct(rig.ctx, rig.tid, d, w)
	require.ErrorIs(t, err, product.ErrNotFound)
	_, err = reader.GetProduct(other.ctx, other.tid, a, w)
	require.ErrorIs(t, err, product.ErrNotFound)

	list := func(f query.ProductListFilter) []string {
		t.Helper()
		views, lerr := reader.ListProducts(rig.ctx, rig.tid, f, w, pagination.Cursor{}, 50)
		require.NoError(t, lerr)
		return viewIDs(views)
	}
	days := func(n int) *int { return &n }
	as, bs, cs := a.String(), b.String(), c.String()

	require.Equal(t, []string{cs, bs, as}, list(query.ProductListFilter{}), "newest first; the other tenant's product is absent")
	require.Equal(t, []string{cs, as}, list(query.ProductListFilter{LowStock: true}), "A: 66 sellable units < 100; B has no reorder level")
	// The last sellable day of the 31 Jul batch is 30 Jun: 37 days from 24 May.
	require.Equal(t, []string{as}, list(query.ProductListFilter{ExpiringWithinDays: days(37)}), "the window is inclusive of the last sellable day")
	require.Empty(t, list(query.ProductListFilter{ExpiringWithinDays: days(36)}))
	require.Equal(t, []string{bs, as}, list(query.ProductListFilter{ExpiringWithinDays: days(400)}))
	require.Equal(t, []string{as}, list(query.ProductListFilter{Category: "cardiac"}), "case-insensitive")
	require.Equal(t, []string{cs, bs}, list(query.ProductListFilter{Category: "General"}))
	require.Equal(t, []string{cs}, list(query.ProductListFilter{Category: "General", LowStock: true}))
	require.Equal(t, []string{bs}, list(query.ProductListFilter{DosageForm: "syrup"}))
	require.Equal(t, []string{bs}, list(query.ProductListFilter{Manufacturer: "zed"}))

	// The list rows carry the same figures as the detail.
	views, err := reader.ListProducts(rig.ctx, rig.tid, query.ProductListFilter{Category: "Cardiac"}, w, pagination.Cursor{}, 50)
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, got, views[0])

	// Keyset: limit, then the rest after the cursor.
	first, err := reader.ListProducts(rig.ctx, rig.tid, query.ProductListFilter{}, w, pagination.Cursor{}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{cs, bs}, viewIDs(first))
	rest, err := reader.ListProducts(rig.ctx, rig.tid, query.ProductListFilter{}, w,
		pagination.Cursor{SortValue: first[1].CreatedAt, ID: first[1].ID}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{as}, viewIDs(rest))
}

// TestSellable_SQLAgreesWithGoAtMonthEndsInIST: for each instant around a month
// end (00:00 IST and 05:30 IST, the two moments a UTC-day implementation gets
// wrong) and each stop setting, the product's available units and the FEFO list
// match what common/expiry.IsSellable says for every batch.
func TestSellable_SQLAgreesWithGoAtMonthEndsInIST(t *testing.T) {
	t.Parallel()
	rig := newStockRig(t)
	pool := repoFixture(t)
	reader := adapters.NewProductReader(pool, pg.NewTransactor(pool))
	p := rig.product(t, product.Spec{DosageForm: "Tablet"})
	var expiries []time.Time
	for m := range 14 {
		e := monthEnd(2026, time.Month(1+m))
		expiries = append(expiries, e)
		rig.batch(t, p, e, 1, 0, 0)
	}

	instants := []time.Time{
		time.Date(2026, 5, 31, 18, 29, 59, 0, time.UTC), // 23:59:59 IST 31 May
		time.Date(2026, 5, 31, 18, 30, 0, 0, time.UTC),  // 00:00 IST 1 Jun
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),     // 05:30 IST 1 Jun
		time.Date(2026, 6, 30, 18, 29, 59, 0, time.UTC),
		time.Date(2026, 6, 30, 18, 30, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 28, 18, 30, 0, 0, time.UTC), // 00:00 IST 1 Mar
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	batches := adapters.NewBatchRepository(pool, pg.NewTransactor(pool))
	for _, now := range instants {
		for _, n := range []int{0, 1, 3} {
			w := windowAt(now, n)
			want := int64(0)
			for _, e := range expiries {
				if expiry.IsSellable(e, w.Today, n) {
					want++
				}
			}
			got, err := reader.GetProduct(rig.ctx, rig.tid, p, w)
			require.NoError(t, err)
			require.Equal(t, want, got.StockAvailable, "available at %s N=%d (business day %s)", now, n, w.Today.Format(time.DateOnly))
			fefo, err := batches.ListFefoForProduct(rig.ctx, rig.tid, p, w.MinSellable)
			require.NoError(t, err)
			require.Len(t, fefo, int(want), "FEFO at %s N=%d", now, n)
			for _, b := range fefo {
				require.True(t, expiry.IsSellable(b.ExpiryDate(), w.Today, n))
			}
		}
	}
}
