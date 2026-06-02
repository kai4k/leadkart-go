package query_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/query"
)

// fakeSearchIndex is an inline minimal [query.SearchIndex].
// Supports: both-categories enabled, DeadlineExceeded → HasPartial, non-deadline error → bubbles.
type fakeSearchIndex struct {
	personsRows  []query.SearchPersonHit
	personsErr   error
	tenantsRows  []query.SearchTenantHit
	tenantsErr   error
	personsCalls atomic.Int32
	tenantsCalls atomic.Int32
	// personsCapturedQ / tenantsCapturedQ record the first q forwarded
	// by the handler (after trim + clamp). Concurrent fanout → first only.
	personsCapturedQ string
	tenantsCapturedQ string
	// personsBlock simulates a slow query; held until released or ctx cancels.
	personsBlock chan struct{}
}

func (f *fakeSearchIndex) SearchPersons(ctx context.Context, q string, _ int32) ([]query.SearchPersonHit, error) {
	f.personsCalls.Add(1)
	if f.personsCapturedQ == "" {
		f.personsCapturedQ = q
	}
	if f.personsBlock != nil {
		select {
		case <-f.personsBlock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.personsErr != nil {
		return nil, f.personsErr
	}
	return f.personsRows, nil
}

func (f *fakeSearchIndex) SearchTenants(_ context.Context, q string, _ int32) ([]query.SearchTenantHit, error) {
	f.tenantsCalls.Add(1)
	if f.tenantsCapturedQ == "" {
		f.tenantsCapturedQ = q
	}
	if f.tenantsErr != nil {
		return nil, f.tenantsErr
	}
	return f.tenantsRows, nil
}

var _ query.SearchIndex = (*fakeSearchIndex)(nil)

// ----- SearchHandler -------------------------------------------------------

func TestNewSearchHandler_PanicsOnNilIndex(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewSearchHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestSearch_RejectsShortQuery(t *testing.T) {
	t.Parallel()
	h := query.NewSearchHandler(&fakeSearchIndex{})
	cases := []struct {
		name string
		q    string
	}{
		{"empty", ""},
		{"one char", "a"},
		{"whitespace-only", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(t.Context(), query.SearchQuery{Q: c.q, IncludePersons: true})
			if !errors.Is(err, query.ErrSearchQueryTooShort) {
				t.Fatalf("err = %v, want ErrSearchQueryTooShort", err)
			}
		})
	}
}

func TestSearch_LongQueryClampedTo100(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{}
	h := query.NewSearchHandler(idx)
	long := strings.Repeat("a", 150)
	if _, err := h.Handle(t.Context(), query.SearchQuery{Q: long, IncludePersons: true}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := idx.personsCapturedQ; len(got) != 100 {
		t.Errorf("captured q len = %d, want 100", len(got))
	}
}

func TestSearch_NoCategoriesEnabled_ReturnsEmptyView(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{}
	h := query.NewSearchHandler(idx)
	view, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(view.Persons) != 0 || len(view.Tenants) != 0 {
		t.Errorf("non-empty view: %+v", view)
	}
	if view.HasPartial {
		t.Errorf("HasPartial = true")
	}
	if idx.personsCalls.Load() != 0 || idx.tenantsCalls.Load() != 0 {
		t.Errorf("index called despite no categories")
	}
}

func TestSearch_PersonsOnly(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		personsRows: []query.SearchPersonHit{{ID: "p1", Email: "a@b.test", FirstName: "A", LastName: "B"}},
	}
	h := query.NewSearchHandler(idx)
	view, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(view.Persons) != 1 || len(view.Tenants) != 0 {
		t.Errorf("rows wrong: %+v", view)
	}
	if idx.tenantsCalls.Load() != 0 {
		t.Errorf("tenants called when not requested")
	}
}

func TestSearch_TenantsOnly(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		tenantsRows: []query.SearchTenantHit{{ID: "t1", Slug: "acme"}},
	}
	h := query.NewSearchHandler(idx)
	view, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludeTenants: true})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(view.Tenants) != 1 || len(view.Persons) != 0 {
		t.Errorf("rows wrong: %+v", view)
	}
	if idx.personsCalls.Load() != 0 {
		t.Errorf("persons called when not requested")
	}
}

func TestSearch_BothCategories_ParallelFanout(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		personsRows: []query.SearchPersonHit{{ID: "p1"}},
		tenantsRows: []query.SearchTenantHit{{ID: "t1"}},
	}
	h := query.NewSearchHandler(idx)
	view, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true, IncludeTenants: true})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(view.Persons) != 1 || len(view.Tenants) != 1 {
		t.Errorf("rows wrong: %+v", view)
	}
}

func TestSearch_PersonsDeadlineExceeded_HasPartial(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{personsErr: context.DeadlineExceeded, tenantsRows: []query.SearchTenantHit{{ID: "t1"}}}
	h := query.NewSearchHandler(idx)
	view, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true, IncludeTenants: true})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !view.HasPartial {
		t.Errorf("HasPartial = false, want true")
	}
	if len(view.Tenants) != 1 {
		t.Errorf("tenants should still populate; got %+v", view)
	}
}

func TestSearch_TenantsDeadlineExceeded_HasPartial(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{tenantsErr: context.DeadlineExceeded, personsRows: []query.SearchPersonHit{{ID: "p1"}}}
	h := query.NewSearchHandler(idx)
	view, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true, IncludeTenants: true})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !view.HasPartial {
		t.Errorf("HasPartial = false, want true")
	}
}

func TestSearch_PersonsError_Bubbles(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("persons boom")
	idx := &fakeSearchIndex{personsErr: sentinel}
	h := query.NewSearchHandler(idx)
	_, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestSearch_TenantsError_Bubbles(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("tenants boom")
	idx := &fakeSearchIndex{tenantsErr: sentinel}
	h := query.NewSearchHandler(idx)
	_, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludeTenants: true})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestSearch_LimitClampDefaults(t *testing.T) {
	t.Parallel()
	// Negative / zero → default (5). Index called; limit not asserted (fake doesn't capture it).
	idx := &fakeSearchIndex{}
	h := query.NewSearchHandler(idx)
	for _, l := range []int{0, -3} {
		if _, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true, PerCategoryLimit: l}); err != nil {
			t.Errorf("limit=%d err: %v", l, err)
		}
	}
}

func TestSearch_LimitClampMax(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{}
	h := query.NewSearchHandler(idx)
	// 9999 should be clamped to 20.
	if _, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true, PerCategoryLimit: 9999}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// ----- CachedSearchHandler ------------------------------------------------

func TestNewCachedSearchHandler_PanicsOnNilCache(t *testing.T) {
	t.Parallel()
	inner := query.NewSearchHandler(&fakeSearchIndex{})
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewCachedSearchHandler(inner, nil) // arch-test:ignore-err - test fixture setup
}

func TestCachedSearch_RejectsShortQuery(t *testing.T) {
	t.Parallel()
	inner := query.NewSearchHandler(&fakeSearchIndex{})
	hc := newHybridCacheForTest(t)
	h := query.NewCachedSearchHandler(inner, hc)
	_, err := h.Handle(t.Context(), query.SearchQuery{Q: "a"})
	if !errors.Is(err, query.ErrSearchQueryTooShort) {
		t.Fatalf("err = %v, want ErrSearchQueryTooShort", err)
	}
}

func TestCachedSearch_HappyPath_CachesView(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{personsRows: []query.SearchPersonHit{{ID: "p1"}}}
	inner := query.NewSearchHandler(idx)
	hc := newHybridCacheForTest(t)
	h := query.NewCachedSearchHandler(inner, hc)

	q := query.SearchQuery{Q: "acme", IncludePersons: true, PerCategoryLimit: 5}
	view, err := h.Handle(t.Context(), q)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(view.Persons) != 1 {
		t.Errorf("persons len = %d", len(view.Persons))
	}
	hc.L1.Wait()
	// Second identical call — cache hit; SearchPersons must not be called again.
	if _, err := h.Handle(t.Context(), q); err != nil {
		t.Fatalf("Handle 2: %v", err)
	}
	if got := idx.personsCalls.Load(); got != 1 {
		t.Errorf("personsCalls = %d, want 1 (cache hit)", got)
	}
}

func TestCachedSearch_LongQueryClampedBeforeCacheKey(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{}
	inner := query.NewSearchHandler(idx)
	hc := newHybridCacheForTest(t)
	h := query.NewCachedSearchHandler(inner, hc)
	long := strings.Repeat("a", 150)
	if _, err := h.Handle(t.Context(), query.SearchQuery{Q: long, IncludePersons: true}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestCachedSearch_LimitClampNegativeAndMax(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{}
	inner := query.NewSearchHandler(idx)
	hc := newHybridCacheForTest(t)
	h := query.NewCachedSearchHandler(inner, hc)
	for _, l := range []int{-1, 0, 9999} {
		if _, err := h.Handle(t.Context(), query.SearchQuery{Q: "acme", IncludePersons: true, PerCategoryLimit: l}); err != nil {
			t.Errorf("limit=%d err: %v", l, err)
		}
	}
}

var _ = time.Second // keeps time import alive
