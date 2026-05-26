package query_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
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

type fakeProductRepo struct {
	mu       sync.Mutex
	products map[product.ID]*product.Product
	getErr   error
	listPage pagination.Page[*product.Product]
	listErr  error
}

func newFakeProductRepo() *fakeProductRepo {
	return &fakeProductRepo{products: make(map[product.ID]*product.Product)}
}

func (r *fakeProductRepo) Add(context.Context, *product.Product) error { return nil }
func (r *fakeProductRepo) UpdateByID(context.Context, tenant.ID, product.ID, func(*product.Product) (bool, error)) error {
	return nil
}

func (r *fakeProductRepo) GetByID(_ context.Context, _ tenant.ID, id product.ID) (*product.Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	p, ok := r.products[id]
	if !ok || p.IsDeleted() {
		return nil, product.ErrNotFound
	}
	return p, nil
}

func (r *fakeProductRepo) ListPage(context.Context, tenant.ID, product.ListFilter, pagination.Cursor, int) (pagination.Page[*product.Product], error) {
	if r.listErr != nil {
		return pagination.Page[*product.Product]{}, r.listErr
	}
	return r.listPage, nil
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
	_ product.Repository       = (*fakeProductRepo)(nil)
	_ batch.Repository         = (*fakeBatchRepo)(nil)
	_ stockmovement.Repository = (*fakeMovementRepo)(nil)
)

var errSentinel = errors.New("sentinel")

// ----- helpers -------------------------------------------------------------

func newTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

func newMembershipID(t *testing.T) membership.ID {
	t.Helper()
	return membership.ID(ids.NewV7().String())
}

// ----- GetProduct ----------------------------------------------------------

func TestGetProductHandler_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "GP-1", Name: "X", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200},
		fixedNow)
	repo.products[p.ID()] = p
	_ = p.PullEvents()

	h := query.NewGetProductHandler(repo)
	got, err := h.Handle(t.Context(), query.GetProductQuery{ProductID: p.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ID() != p.ID() {
		t.Fatalf("id mismatch")
	}
}

func TestGetProductHandler_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := query.NewGetProductHandler(repo)
	_, err := h.Handle(t.Context(), query.GetProductQuery{ProductID: product.ID("nope")})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestGetProductHandler_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	repo.getErr = errSentinel
	h := query.NewGetProductHandler(repo)
	_, err := h.Handle(t.Context(), query.GetProductQuery{ProductID: product.ID("any")})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// ----- ListProductsPage ----------------------------------------------------

func TestListProductsPageHandler_HappyPath_PassesFilter(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	repo.listPage = pagination.Page[*product.Product]{Items: []*product.Product{}}
	h := query.NewListProductsPageHandler(repo)
	tid := newTenantID(t)

	page, err := h.Handle(t.Context(), query.ListProductsPageQuery{
		TenantID: tid, PageSize: 25, ActiveOnly: true,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if page.Items == nil {
		t.Fatal("Items: nil — expected non-nil slice from BuildPage shape")
	}
}

func TestListProductsPageHandler_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	repo.listErr = errSentinel
	h := query.NewListProductsPageHandler(repo)
	tid := newTenantID(t)

	_, err := h.Handle(t.Context(), query.ListProductsPageQuery{
		TenantID: tid, PageSize: 25,
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// ----- GetBatch ------------------------------------------------------------

func TestGetBatchHandler_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	h := query.NewGetBatchHandler(repo)
	_, err := h.Handle(t.Context(), query.GetBatchQuery{BatchID: batch.ID("nope")})
	if !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

func TestGetBatchHandler_RepoError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeBatchRepo()
	repo.getErr = errSentinel
	h := query.NewGetBatchHandler(repo)
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
	h := query.NewListBatchesByProductHandler(repo)
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
	h := query.NewListBatchesByProductHandler(repo)
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
