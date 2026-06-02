package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/leadkart/leadkart-go/internal/common/cache"
)

// SearchView is the omni-search response (GET /v1/search?q=, ADR 0040).
// Parallel pg_trgm fanout across resource types; operator-only path.
// HasPartial = true when ≥1 sub-query exceeded its per-category timeout.
type SearchView struct {
	Persons    []SearchPersonHit
	Tenants    []SearchTenantHit
	HasPartial bool
}

// SearchPersonHit is one row in the persons category.
type SearchPersonHit struct {
	ID        string
	Email     string
	FirstName string
	LastName  string
	CreatedAt time.Time
}

// SearchTenantHit is one row in the tenants category.
type SearchTenantHit struct {
	ID          string
	Slug        string
	LegalName   string
	DisplayName string
	Status      string
	CreatedAt   time.Time
}

// SearchQuery carries the validated request inputs.
// Q is clamped to [2, 100] chars; PerCategoryLimit defaults to 5.
type SearchQuery struct {
	Q                string
	PerCategoryLimit int
	IncludePersons   bool
	IncludeTenants   bool
}

// ErrSearchQueryTooShort surfaces when the caller's query string
// has fewer than 2 trimmed characters. HTTP maps to 400.
var ErrSearchQueryTooShort = errors.New("search: query too short (min 2 chars)")

// Search parameter clamps. Closed-set per ADR 0040 (bounded cache key space).
const (
	searchQueryMinLen          = 2
	searchQueryMaxLen          = 100
	searchDefaultCategoryLimit = 5
	searchMaxCategoryLimit     = 20
	// Per-category timeout; exceeded sub-searches yield HasPartial=true.
	searchPerCategoryTimeout = 200 * time.Millisecond
)

// SearchIndex is the consumer-defined interface for parallel pg_trgm fanout
// (ADR 0047: no pgx/pgxpool/sqlc in app/). Each method runs under
// platform-scope tx; on context.DeadlineExceeded must return (nil, DeadlineExceeded)
// so the handler can set HasPartial=true instead of failing.
type SearchIndex interface {
	SearchPersons(ctx context.Context, q string, limit int32) ([]SearchPersonHit, error)
	SearchTenants(ctx context.Context, q string, limit int32) ([]SearchTenantHit, error)
}

// SearchHandler depends on [SearchIndex] only (ADR 0047).
type SearchHandler struct {
	index SearchIndex
}

// NewSearchHandler wires the un-cached handler.
func NewSearchHandler(index SearchIndex) SearchHandler {
	if index == nil {
		panic("query: NewSearchHandler search index required")
	}
	return SearchHandler{index: index}
}

// Handle runs the parallel fanout. Validates and clamps q and limit,
// fans out to persons + tenants concurrently; DeadlineExceeded on any
// sub-query sets HasPartial=true rather than failing the whole call.
func (h SearchHandler) Handle(ctx context.Context, q SearchQuery) (SearchView, error) {
	q.Q = strings.TrimSpace(q.Q)
	if len(q.Q) < searchQueryMinLen {
		return SearchView{}, ErrSearchQueryTooShort
	}
	if len(q.Q) > searchQueryMaxLen {
		q.Q = q.Q[:searchQueryMaxLen]
	}

	limit := q.PerCategoryLimit
	if limit <= 0 {
		limit = searchDefaultCategoryLimit
	}
	if limit > searchMaxCategoryLimit {
		limit = searchMaxCategoryLimit
	}
	//nolint:gosec // limit clamped at 20 above
	limit32 := int32(limit)

	// Each goroutine writes only its own locals; view assembled after Wait.
	// (Earlier version mutated a shared SearchView, racing on HasPartial.)
	var (
		persons        []SearchPersonHit
		tenants        []SearchTenantHit
		personsPartial bool
		tenantsPartial bool
	)

	g, gctx := errgroup.WithContext(ctx)
	if q.IncludePersons {
		g.Go(func() error {
			subCtx, cancel := context.WithTimeout(gctx, searchPerCategoryTimeout)
			defer cancel()
			rows, err := h.index.SearchPersons(subCtx, q.Q, limit32)
			if errors.Is(err, context.DeadlineExceeded) {
				personsPartial = true
				return nil
			}
			if err != nil {
				return fmt.Errorf("search: persons: %w", err)
			}
			persons = rows
			return nil
		})
	}

	if q.IncludeTenants {
		g.Go(func() error {
			subCtx, cancel := context.WithTimeout(gctx, searchPerCategoryTimeout)
			defer cancel()
			rows, err := h.index.SearchTenants(subCtx, q.Q, limit32)
			if errors.Is(err, context.DeadlineExceeded) {
				tenantsPartial = true
				return nil
			}
			if err != nil {
				return fmt.Errorf("search: tenants: %w", err)
			}
			tenants = rows
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return SearchView{}, err
	}
	return SearchView{
		Persons:    persons,
		Tenants:    tenants,
		HasPartial: personsPartial || tenantsPartial,
	}, nil
}

// CachedSearchHandler wraps SearchHandler with cache.SearchResultsTTL.
// Cache key includes normalized query, category flags, and limit.
type CachedSearchHandler struct {
	facade *cache.Facade[searchCacheKey, SearchView]
}

// searchCacheKey scopes the cache key to the request shape that
// affects results.
type searchCacheKey struct {
	Q                string
	IncludePersons   bool
	IncludeTenants   bool
	PerCategoryLimit int
}

// NewCachedSearchHandler builds the cache facade (ADR 0042: 30s L1 / 5min L2 + jitter).
func NewCachedSearchHandler(inner SearchHandler, hc *cache.HybridCache) CachedSearchHandler {
	if hc == nil {
		panic("query: NewCachedSearchHandler hybrid cache required")
	}
	facade := cache.NewFacade[searchCacheKey, SearchView](
		hc, "search",
		func(k searchCacheKey) string {
			// Include category flags in the key to prevent cross-category cache hits.
			persons := "0"
			if k.IncludePersons {
				persons = "1"
			}
			tenants := "0"
			if k.IncludeTenants {
				tenants = "1"
			}
			return fmt.Sprintf("leadkart:search:q=%s:p=%s:t=%s:l=%d",
				strings.ToLower(k.Q), persons, tenants, k.PerCategoryLimit)
		},
		func(ctx context.Context, k searchCacheKey) (SearchView, error) {
			return inner.Handle(ctx, SearchQuery{
				Q:                k.Q,
				PerCategoryLimit: k.PerCategoryLimit,
				IncludePersons:   k.IncludePersons,
				IncludeTenants:   k.IncludeTenants,
			})
		},
		cache.WithTTL(cache.SearchResultsTTL()),
	)
	return CachedSearchHandler{facade: facade}
}

// Handle dispatches through the cache facade; revalidates q as belt-and-braces.
func (h CachedSearchHandler) Handle(ctx context.Context, q SearchQuery) (SearchView, error) {
	q.Q = strings.TrimSpace(q.Q)
	if len(q.Q) < searchQueryMinLen {
		return SearchView{}, ErrSearchQueryTooShort
	}
	if len(q.Q) > searchQueryMaxLen {
		q.Q = q.Q[:searchQueryMaxLen]
	}
	limit := q.PerCategoryLimit
	if limit <= 0 {
		limit = searchDefaultCategoryLimit
	}
	if limit > searchMaxCategoryLimit {
		limit = searchMaxCategoryLimit
	}
	return h.facade.Get(ctx, searchCacheKey{
		Q:                strings.ToLower(q.Q),
		IncludePersons:   q.IncludePersons,
		IncludeTenants:   q.IncludeTenants,
		PerCategoryLimit: limit,
	})
}
