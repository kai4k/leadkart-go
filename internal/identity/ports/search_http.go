package ports

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// handleSearch serves GET /api/v1/search?q=...&limit=...
//
// Operator omni-search per ADR 0040 (multi-stage retrieval funnel).
// Query string parameters:
//
//   - ?q=<text>                 — REQUIRED, 2-100 chars after trim
//   - ?limit=<int>              — per-category cap; default 5, max 20
//   - ?include=persons,tenants  — comma-separated; default: every category the
//     caller is authorised for (persons: platform.users.view, tenants:
//     platform.tenants.view); an explicit category the caller may not see is 403
//
// Categories returned:
//
//   - persons  — global identity match (email + first/last name)
//   - tenants  — global tenant match (slug + legal/display name)
//
// Auth: platform anchor AND any of platform.tenants.view / platform.users.view
// at the route; each category is then gated on its own permission here.
// Tenant-scoped omni-search (for tenant admins searching their own
// users) lands in a v0.3 follow-up via a JOIN-based query against
// the existing trigram + composite indexes.
//
// Response: SearchResponse with the two categories + has_partial
// flag. Per-category timeout 200ms; if any sub-query times out the
// response is partial (other categories still surface) and
// has_partial=true.
//
// Cached server-side via cache.SearchResultsTTL (30s L1 / 5min L2 +
// ±10% jitter per ADR 0042). Cache key includes (q, includes, limit)
// so different category combinations don't share cache entries.
func handleSearch(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		// Limit parsing — invalid / negative falls through to handler
		// default (5). Above 20 gets capped to 20.
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

		// Each category needs its own permission. No ?include= returns the
		// authorised subset; an explicitly requested category the caller may not
		// see is a 403 naming it. The query below only ever runs for the
		// authorised categories, so its cache key (which includes the category
		// flags) can never hand one caller another's categories.
		mayPersons := authn.Satisfies(r.Context(), permission.IdentityPermissions.Platform.UsersView)
		mayTenants := authn.Satisfies(r.Context(), permission.IdentityPermissions.Platform.TenantsView)
		includePersons, includeTenants := mayPersons, mayTenants
		if inc := r.URL.Query().Get("include"); inc != "" {
			wantPersons, wantTenants := false, false
			for _, cat := range splitCSV(inc) {
				switch cat {
				case "persons":
					wantPersons = true
				case "tenants":
					wantTenants = true
				}
			}
			if !wantPersons && !wantTenants {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidSearchInclude,
					"at least one of include=persons|tenants is required")
				return
			}
			if wantPersons && !mayPersons {
				writeError(w, http.StatusForbidden, ErrCodeSearchCategoryForbidden,
					"include=persons requires "+permission.IdentityPermissions.Platform.UsersView)
				return
			}
			if wantTenants && !mayTenants {
				writeError(w, http.StatusForbidden, ErrCodeSearchCategoryForbidden,
					"include=tenants requires "+permission.IdentityPermissions.Platform.TenantsView)
				return
			}
			includePersons, includeTenants = wantPersons, wantTenants
		}
		if !includePersons && !includeTenants {
			// Unreachable behind the route gate (it needs one of the two); fail closed.
			writeError(w, http.StatusForbidden, ErrCodeSearchCategoryForbidden, "no searchable category is authorised")
			return
		}

		view, err := a.Queries.Search.Handle(r.Context(), query.SearchQuery{
			Q:                q,
			PerCategoryLimit: limit,
			IncludePersons:   includePersons,
			IncludeTenants:   includeTenants,
		})
		if errors.Is(err, query.ErrSearchQueryTooShort) {
			writeError(w, http.StatusBadRequest, ErrCodeSearchQueryTooShort,
				"q parameter must be at least 2 characters")
			return
		}
		if err != nil {
			log.ErrorContext(r.Context(), "search failed", "err", err, "q_len", len(q))
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		// Project to wire shape — always emit non-nil slices so the
		// frontend can iterate without nil checks (Stripe / Auth0
		// convention).
		resp := SearchResponse{
			Persons:    make([]SearchPersonHit, 0, len(view.Persons)),
			Tenants:    make([]SearchTenantHit, 0, len(view.Tenants)),
			HasPartial: view.HasPartial,
		}
		for _, p := range view.Persons {
			resp.Persons = append(resp.Persons, SearchPersonHit{
				ID:        p.ID,
				Email:     p.Email,
				FirstName: p.FirstName,
				LastName:  p.LastName,
				CreatedAt: p.CreatedAt,
			})
		}
		for _, t := range view.Tenants {
			resp.Tenants = append(resp.Tenants, SearchTenantHit{
				ID:          t.ID,
				Slug:        t.Slug,
				LegalName:   t.LegalName,
				DisplayName: t.DisplayName,
				Status:      t.Status,
				CreatedAt:   t.CreatedAt,
			})
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// splitCSV splits a comma-separated query parameter into trimmed,
// lowercased tokens; empty tokens dropped. Local helper — saves
// pulling strings.Split + manual trim/filter per call site.
func splitCSV(s string) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			tok := s[start:i]
			// trim spaces
			for len(tok) > 0 && (tok[0] == ' ' || tok[0] == '\t') {
				tok = tok[1:]
			}
			for len(tok) > 0 && (tok[len(tok)-1] == ' ' || tok[len(tok)-1] == '\t') {
				tok = tok[:len(tok)-1]
			}
			if tok != "" {
				// lowercase
				lower := make([]byte, len(tok))
				for j := 0; j < len(tok); j++ {
					c := tok[j]
					if c >= 'A' && c <= 'Z' {
						c += 32
					}
					lower[j] = c
				}
				out = append(out, string(lower))
			}
			start = i + 1
		}
	}
	return out
}
