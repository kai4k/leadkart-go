package query_test

import (
	"context"
	"errors"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// fixedNow is the deterministic instant query tests pass to domain
// factories per the clock-injection refactor.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// ----- minimal fakes mirroring the command-side shape ----------------------

// fakeProductReader is the read model the product handlers ask; it records
// what it was asked so a test can check the filter, the date and the limit.
type fakeProductReader struct {
	mu        sync.Mutex
	views     []query.ProductView
	err       error
	gotFilter query.ProductListFilter
	gotToday  time.Time
	gotLimit  int
}

func (r *fakeProductReader) GetProduct(_ context.Context, tid tenant.ID, id product.ID, w sellability.Window) (query.ProductView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gotToday = w.Today
	if r.err != nil {
		return query.ProductView{}, r.err
	}
	for _, v := range r.views {
		if v.ID == id.String() && v.TenantID == tid.String() {
			return v, nil
		}
	}
	return query.ProductView{}, product.ErrNotFound
}

func (r *fakeProductReader) ListProducts(_ context.Context, _ tenant.ID, f query.ProductListFilter, w sellability.Window, _ pagination.Cursor, limit int) ([]query.ProductView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gotFilter, r.gotToday, r.gotLimit = f, w.Today, limit
	if r.err != nil {
		return nil, r.err
	}
	return r.views[:min(limit, len(r.views))], nil
}

type fakeBatchRepo struct {
	mu       sync.Mutex
	batches  map[batch.ID]*batch.Batch
	getErr   error
	listPage pagination.Page[*batch.Batch]
	listErr  error
}

func newFakeBatchRepo() *fakeBatchRepo {
	return &fakeBatchRepo{batches: make(map[batch.ID]*batch.Batch)}
}

func (r *fakeBatchRepo) Add(context.Context, *batch.Batch) error { return nil }
func (r *fakeBatchRepo) UpdateByID(context.Context, tenant.ID, batch.ID, func(*batch.Batch) (bool, error)) error {
	return nil
}

func (r *fakeBatchRepo) GetByID(_ context.Context, _ tenant.ID, id batch.ID) (*batch.Batch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	b, ok := r.batches[id]
	if !ok || b.IsDeleted() {
		return nil, batch.ErrNotFound
	}
	return b, nil
}

func (r *fakeBatchRepo) ListByProductPage(_ context.Context, _ tenant.ID, _ product.ID, _ batch.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*batch.Batch], error) {
	if r.listErr != nil {
		return pagination.Page[*batch.Batch]{}, r.listErr
	}
	return r.listPage, nil
}

func (r *fakeBatchRepo) AnyLiveWithStockForProduct(context.Context, tenant.ID, product.ID) (bool, error) {
	return false, nil
}

func (r *fakeBatchRepo) ListFefoForProduct(_ context.Context, _ tenant.ID, _ product.ID, _ time.Time) ([]*batch.Batch, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.listPage.Items, nil
}

type fakeMovementRepo struct {
	listPage pagination.Page[*stockmovement.Movement]
	listErr  error
}

func (r *fakeMovementRepo) Add(context.Context, *stockmovement.Movement) error { return nil }
func (r *fakeMovementRepo) GetByID(context.Context, tenant.ID, stockmovement.ID) (*stockmovement.Movement, error) {
	return nil, stockmovement.ErrNotFound
}
func (r *fakeMovementRepo) ListByBatchPage(_ context.Context, _ tenant.ID, _ batch.ID, _ stockmovement.PageRequest) (pagination.Page[*stockmovement.Movement], error) {
	if r.listErr != nil {
		return pagination.Page[*stockmovement.Movement]{}, r.listErr
	}
	return r.listPage, nil
}

// compile-time interface satisfaction
var (
	_ query.ProductReader      = (*fakeProductReader)(nil)
	_ batch.Repository         = (*fakeBatchRepo)(nil)
	_ stockmovement.Repository = (*fakeMovementRepo)(nil)
)

var errSentinel = errors.New("sentinel")

// ----- helpers -------------------------------------------------------------

func newTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

// ----- GetProduct ----------------------------------------------------------

func clock() sellability.Resolver { return sellabilitytest.Resolver(fixedNow) }

func TestGetProductHandler_HappyPath(t *testing.T) {
	t.Parallel()
	tid := newTenantID(t)
	expiry := fixedNow.AddDate(0, 1, 0)
	want := query.ProductView{
		ID: ids.NewV7().String(), TenantID: tid.String(), SKU: "GP-1",
		StockOnHand: 60, StockReserved: 15, StockAvailable: 45, EarliestExpiry: &expiry,
	}
	reader := &fakeProductReader{views: []query.ProductView{want}}

	got, err := query.NewGetProductHandler(reader, clock()).Handle(t.Context(), query.GetProductQuery{TenantID: tid, ProductID: product.ID(want.ID)})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if !reader.gotToday.Equal(time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry judged at %v, want the handler's clock %v", reader.gotToday, time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC))
	}
}

func TestGetProductHandler_NotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetProductHandler(&fakeProductReader{}, clock())
	_, err := h.Handle(t.Context(), query.GetProductQuery{ProductID: product.ID("nope")})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestGetProductHandler_ReaderError_Propagates(t *testing.T) {
	t.Parallel()
	h := query.NewGetProductHandler(&fakeProductReader{err: errSentinel}, clock())
	_, err := h.Handle(t.Context(), query.GetProductQuery{ProductID: product.ID("any")})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// ----- ListProductsPage ----------------------------------------------------

func TestListProductsPageHandler_PassesFilterAndClockAndAsksOneExtraRow(t *testing.T) {
	t.Parallel()
	reader := &fakeProductReader{}
	days := 30
	filter := query.ProductListFilter{ActiveOnly: true, Category: "Cardiac", LowStock: true, ExpiringWithinDays: &days}

	page, err := query.NewListProductsPageHandler(reader, clock()).Handle(t.Context(), query.ListProductsPageQuery{
		TenantID: newTenantID(t), PageSize: 25, Filter: filter,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if page.Items == nil || page.HasMore {
		t.Fatalf("empty list: items %v has_more %v; want an empty non-nil slice and no next page", page.Items, page.HasMore)
	}
	if reader.gotFilter != filter {
		t.Fatalf("filter reached the reader as %+v, want %+v", reader.gotFilter, filter)
	}
	if reader.gotLimit != 26 || !reader.gotToday.Equal(time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("limit %d today %v; want 26 (page size + 1) and %v", reader.gotLimit, reader.gotToday, time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC))
	}
}

func TestListProductsPageHandler_ExtraRowBecomesHasMore(t *testing.T) {
	t.Parallel()
	reader := &fakeProductReader{}
	for i := range 3 {
		reader.views = append(reader.views, query.ProductView{
			ID: ids.NewV7().String(), CreatedAt: fixedNow.Add(-time.Duration(i) * time.Minute),
		})
	}

	page, err := query.NewListProductsPageHandler(reader, clock()).Handle(t.Context(), query.ListProductsPageQuery{
		TenantID: newTenantID(t), PageSize: 2,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("items %d has_more %v cursor %q; want 2, true and a cursor", len(page.Items), page.HasMore, page.NextCursor)
	}
	cur, err := pagination.Decode(page.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if cur.ID != reader.views[1].ID || !cur.SortValue.Equal(reader.views[1].CreatedAt) {
		t.Fatalf("cursor %+v does not point at the last row of the page", cur)
	}
}

func TestListProductsPageHandler_ReaderError_Propagates(t *testing.T) {
	t.Parallel()
	h := query.NewListProductsPageHandler(&fakeProductReader{err: errSentinel}, clock())
	_, err := h.Handle(t.Context(), query.ListProductsPageQuery{TenantID: newTenantID(t), PageSize: 25})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// ----- GetBatch ------------------------------------------------------------

func TestGetBatchHandler_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	h := query.NewGetBatchHandler(repo, clock())
	_, err := h.Handle(t.Context(), query.GetBatchQuery{BatchID: batch.ID("nope")})
	if !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestGetBatchHandler_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	repo.getErr = errSentinel
	h := query.NewGetBatchHandler(repo, clock())
	_, err := h.Handle(t.Context(), query.GetBatchQuery{BatchID: batch.ID("any")})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// ----- ListBatchesByProduct ------------------------------------------------

func TestListBatchesByProductHandler_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	repo.listPage = pagination.Page[*batch.Batch]{Items: []*batch.Batch{}}
	h := query.NewListBatchesByProductHandler(repo, clock())
	_, err := h.Handle(t.Context(), query.ListBatchesByProductQuery{
		ProductID: product.ID("p"), PageSize: 10,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestListBatchesByProductHandler_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	repo.listErr = errSentinel
	h := query.NewListBatchesByProductHandler(repo, clock())
	_, err := h.Handle(t.Context(), query.ListBatchesByProductQuery{
		ProductID: product.ID("p"), PageSize: 10,
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// ----- ListBatchMovementsPage ----------------------------------------------

func TestListBatchMovementsPageHandler_HappyPath(t *testing.T) {
	t.Parallel()
	repo := &fakeMovementRepo{listPage: pagination.Page[*stockmovement.Movement]{Items: []*stockmovement.Movement{}}}
	h := query.NewListBatchMovementsPageHandler(repo)
	_, err := h.Handle(t.Context(), query.ListBatchMovementsPageQuery{
		BatchID: batch.ID("b"), PageSize: 25,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestListBatchMovementsPageHandler_InvalidType_ReturnsErrInvalid(t *testing.T) {
	t.Parallel()
	repo := &fakeMovementRepo{}
	h := query.NewListBatchMovementsPageHandler(repo)
	_, err := h.Handle(t.Context(), query.ListBatchMovementsPageQuery{
		BatchID:  batch.ID("b"),
		PageSize: 25,
		Type:     batch.MovementType("garbage"),
	})
	if !errors.Is(err, stockmovement.ErrInvalid) {
		t.Fatalf("err: got %v want stockmovement.ErrInvalid", err)
	}
}

func TestListBatchMovementsPageHandler_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	repo := &fakeMovementRepo{listErr: errSentinel}
	h := query.NewListBatchMovementsPageHandler(repo)
	_, err := h.Handle(t.Context(), query.ListBatchMovementsPageQuery{
		BatchID:  batch.ID("b"),
		PageSize: 25,
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// ----- FefoBatches ---------------------------------------------------------

func TestFefoBatchesHandler_HappyPath_ReturnsItems(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	repo.listPage = pagination.Page[*batch.Batch]{Items: []*batch.Batch{}}
	h := query.NewFefoBatchesHandler(repo, clock())
	_, err := h.Handle(t.Context(), query.FefoBatchesQuery{
		TenantID: newTenantID(t), ProductID: product.ID("p"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestFefoBatchesHandler_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	repo.listErr = errSentinel
	h := query.NewFefoBatchesHandler(repo, clock())
	_, err := h.Handle(t.Context(), query.FefoBatchesQuery{
		TenantID: newTenantID(t), ProductID: product.ID("p"),
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel", err)
	}
}

// ----- ListProductCategories -------------------------------------------------

type fakeCategoryReader struct {
	out []query.ProductCategoryView
	err error
}

func (f fakeCategoryReader) ListProductCategories(context.Context) ([]query.ProductCategoryView, error) {
	return f.out, f.err
}

func TestListProductCategoriesHandler_ReturnsTheReferenceList(t *testing.T) {
	t.Parallel()
	want := []query.ProductCategoryView{{Category: "General"}, {Category: "Nutraceuticals"}}
	got, err := query.NewListProductCategoriesHandler(fakeCategoryReader{out: want}).Handle(t.Context(), query.ListProductCategoriesQuery{})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListProductCategoriesHandler_ReaderError_Propagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	_, err := query.NewListProductCategoriesHandler(fakeCategoryReader{err: boom}).Handle(t.Context(), query.ListProductCategoriesQuery{})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v, want it to wrap the reader's error", err)
	}
}

// A batch read carries the reserved quantity next to on hand, so a screen can
// show what can really be sold or adjusted.
func TestGetBatchHandler_CarriesQuantityReserved(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	tid := newTenantID(t)
	id := batch.ID(ids.NewV7().String())
	repo.batches[id] = batch.UnmarshalFromDB(batch.Snapshot{
		ID: id, TenantID: tid, ProductID: product.ID(ids.NewV7().String()),
		QuantityOnHand: 40, QuantityReserved: 15,
	})

	got, err := query.NewGetBatchHandler(repo, clock()).Handle(t.Context(), query.GetBatchQuery{TenantID: tid, BatchID: id})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.QuantityOnHand != 40 || got.QuantityReserved != 15 {
		t.Fatalf("on hand %d reserved %d; want 40 and 15", got.QuantityOnHand, got.QuantityReserved)
	}
}

// ----- order hold expiry ----------------------------------------------------

type fakeHeldStock struct {
	n        int64
	err      error
	gotToday time.Time
	gotRef   string
}

func (f *fakeHeldStock) CountExpiredBatchesHeldByOrder(_ context.Context, _ tenant.ID, ref string, today time.Time) (int64, error) {
	f.gotToday, f.gotRef = today, ref
	return f.n, f.err
}

// The question is judged on the company business day, not the UTC day: 18:30 UTC is
// already the next day at the default offset, which is what flips a month-end batch.
func TestOrderHoldExpiry_JudgesOnTheBusinessDay(t *testing.T) {
	t.Parallel()
	reader := &fakeHeldStock{n: 1}
	h := query.NewOrderHoldExpiryHandler(reader, sellabilitytest.Resolver(time.Date(2027, 2, 28, 18, 30, 0, 0, time.UTC)))

	got, err := h.Handle(t.Context(), query.OrderHoldExpiryQuery{TenantID: "t", OrderID: "order-1"})

	if err != nil || !got {
		t.Fatalf("got %v, %v; want true, nil", got, err)
	}
	if want := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC); !reader.gotToday.Equal(want) {
		t.Fatalf("asked for %v; want the business day %v", reader.gotToday, want)
	}
	if reader.gotRef != "order-1" {
		t.Fatalf("asked about %q", reader.gotRef)
	}
}

func TestOrderHoldExpiry_NothingExpiredAndErrors(t *testing.T) {
	t.Parallel()
	res := sellabilitytest.Resolver(fixedNow)
	if got, err := query.NewOrderHoldExpiryHandler(&fakeHeldStock{}, res).Handle(t.Context(), query.OrderHoldExpiryQuery{TenantID: "t", OrderID: "o"}); err != nil || got {
		t.Fatalf("no expired batch: got %v, %v; want false, nil", got, err)
	}
	boom := errors.New("db down")
	if _, err := query.NewOrderHoldExpiryHandler(&fakeHeldStock{err: boom}, res).Handle(t.Context(), query.OrderHoldExpiryQuery{TenantID: "t", OrderID: "o"}); !errors.Is(err, boom) {
		t.Fatalf("reader error lost: %v", err)
	}
	if _, err := query.NewOrderHoldExpiryHandler(&fakeHeldStock{}, res).Handle(t.Context(), query.OrderHoldExpiryQuery{OrderID: "o"}); err == nil {
		t.Fatal("tenant required")
	}
	if _, err := query.NewOrderHoldExpiryHandler(&fakeHeldStock{}, res).Handle(t.Context(), query.OrderHoldExpiryQuery{TenantID: "t"}); err == nil {
		t.Fatal("order required")
	}
}
