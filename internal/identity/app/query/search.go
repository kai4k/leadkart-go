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

// SearchView is the omni-search response (GET /v1/search?q=).
//
// Per ADR 0040 — multi-stage retrieval funnel: stage 1 is parallel
// pg_trgm fanout across resource types; stage 2 is per-resource
// similarity() ranking. Cmd+K UX returns categorized buckets so the
// frontend can render groups separately.
//
// Currently surfaces persons + tenants (operator-only path). Future
// extensions: users (per-tenant via memberships JOIN), leads (Phase 2).
//
// HasPartial = true means ≥1 sub-query exceeded the per-query
// timeout. Frontend renders what's returned + signals partial data.
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
//
// Q is bound to [2, 100] chars at the HTTP boundary. PerCategoryLimit
// caps the per-resource result count (defaults to 5).
type SearchQuery struct {
	Q                 string
	PerCategoryLimit  int
	IncludePersons    bool
	IncludeTenants    bool
}

// ErrSearchQueryTooShort surfaces when the caller's query string
// has fewer than 2 trimmed characters. HTTP maps to 400.
var ErrSearchQueryTooShort = errors.New("search: query too short (min 2 chars)")

// Search parameter clamps. Closed-set per ADR 0040 — bounded so
// cache key space is bounded.
const (
	searchQueryMinLen          = 2
	searchQueryMaxLen          = 100
	searchDefaultCategoryLimit = 5
	searchMaxCategoryLimit     = 20
	// Per-category timeout. Cmd+K UX needs sub-second total response
	// time; if one resource search blows past this, return partial
	// results rather than blocking the entire response.
	searchPerCategoryTimeout = 200 * time.Millisecond
)

// SearchIndex is the consumer-defined contract for the parallel
// pg_trgm fanout. Defined here (next to its sole consumer
// [SearchHandler]) per Cheney "accept interfaces"; concrete pg-backed
// implementation lives in internal/identity/adapters/ where db.* is
// permitted. This layer carries NO pgx, pgxpool, or sqlc imports.
//
// Each method runs under platform-scope tx with the supplied
// per-category timeout. On context.DeadlineExceeded the implementation
// MUST return ([], context.DeadlineExceeded) — the handler turns that
// into HasPartial=true rather than a hard failure.
type SearchIndex interface {
	SearchPersons(ctx context.Context, q string, limit int32) ([]SearchPersonHit, error)
	SearchTenants(ctx context.Context, q string, limit int32) ([]SearchTenantHit, error)
}

// SearchHandler depends on [SearchIndex] only — no pgxpool, no pgx,
// no sqlc. Boundary discipline per ADR 0047.
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

// Handle runs the fanout. errgroup coordinates the parallel sub-
// queries; each carries a per-category timeout so a single slow
// sub-search doesn't block the whole response.
//
// Order of operations:
//
//  1. Validate q (length 2-100 chars; reject early on too-short).
//  2. Clamp per-category limit.
//  3. Fan out (persons + tenants) in parallel.
//  4. Each sub-query has its own timeout-bounded ctx; on timeout the
//     handler returns partial results with HasPartial=true.
//  5. Compose + return.
//
// Caller (HTTP layer) is responsible for converting timeouts /
// partial states to the wire-shape — this handler just signals via
// HasPartial.
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

	// Each goroutine writes ONLY its own locals; nothing is shared, so
	// there is no data race. The view is assembled after Wait. (The
	// earlier version mutated one shared SearchView from both goroutines,
	// which raced on HasPartial when both categories timed out.)
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
// Cache key includes the normalized query + bitmask of included
// categories + limit (full inputs → cache key collision avoidance).
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

// NewCachedSearchHandler builds the facade. SearchResultsTTL = 30s
// L1 / 5min L2 + ±10% jitter per ADR 0042 — typing burst tolerant,
// stampede-protected.
func NewCachedSearchHandler(inner SearchHandler, hc *cache.HybridCache) CachedSearchHandler {
	if hc == nil {
		panic("query: NewCachedSearchHandler hybrid cache required")
	}
	facade := cache.NewFacade[searchCacheKey, SearchView](
		hc, "search",
		func(k searchCacheKey) string {
			// Include the include-flags in the key so a request that
			// asks for persons-only doesn't return a tenants-included
			// cached entry (or vice versa).
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

// Handle dispatches through the cache facade. The HTTP boundary
// must validate q (length + non-empty) before calling — the
// underlying SearchHandler revalidates as a belt-and-braces.
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
