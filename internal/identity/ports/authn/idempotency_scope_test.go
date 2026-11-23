package authn_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const (
	scopeTenantA = "0190a000-0000-7000-8000-00000000000a"
	scopeTenantB = "0190a000-0000-7000-8000-00000000000b"
	scopePerson1 = "0190a000-0000-7000-8000-000000000001"
	scopePerson2 = "0190a000-0000-7000-8000-000000000002"
)

// tokenVerifier accepts a fixed set of tokens, each mapped to claims.
type tokenVerifier map[string]*jwt.Claims

func (v tokenVerifier) Verify(token string) (*jwt.Claims, error) {
	if c, ok := v[token]; ok {
		return c, nil
	}
	return nil, io.ErrUnexpectedEOF
}

func scopeClaims(tenantID, person string) *jwt.Claims {
	c := &jwt.Claims{TenantID: tenantID, MembershipID: person + "-m"}
	c.Subject = person
	return c
}

func scopeOf(t *testing.T, k idempotency.CallerKeyer, method, path, bearer, remote, xff string) string {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	r.RemoteAddr = remote
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return k(r)
}

func TestIdempotencyCallerKeyer_ScopesByVerifiedTenantPersonAndRoute(t *testing.T) {
	t.Parallel()
	k := authn.IdempotencyCallerKeyer(tokenVerifier{
		"a1":  scopeClaims(scopeTenantA, scopePerson1),
		"a1b": scopeClaims(scopeTenantA, scopePerson1), // same caller, newer token
		"a2":  scopeClaims(scopeTenantA, scopePerson2),
		"b1":  scopeClaims(scopeTenantB, scopePerson1),
	})

	base := scopeOf(t, k, "POST", "/api/v1/leads", "a1", "127.0.0.1:1", "")
	if !strings.HasPrefix(base, "tenant:"+scopeTenantA+":") {
		t.Fatalf("scope %q does not start with the tenant namespace", base)
	}
	if got := scopeOf(t, k, "POST", "/api/v1/leads", "a1b", "127.0.0.1:1", ""); got != base {
		t.Fatalf("a refreshed token of the same caller must keep its scope: %q != %q", got, base)
	}
	if len(base) > idempotency.MaxCallerIDLen {
		t.Fatalf("scope %d bytes exceeds the store limit", len(base))
	}
	for name, other := range map[string]string{
		"another person, same tenant": scopeOf(t, k, "POST", "/api/v1/leads", "a2", "127.0.0.1:1", ""),
		"same person, another tenant": scopeOf(t, k, "POST", "/api/v1/leads", "b1", "127.0.0.1:1", ""),
		"another route":               scopeOf(t, k, "POST", "/api/v1/orders", "a1", "127.0.0.1:1", ""),
		"another method":              scopeOf(t, k, "PATCH", "/api/v1/leads", "a1", "127.0.0.1:1", ""),
		"anonymous":                   scopeOf(t, k, "POST", "/api/v1/leads", "", "127.0.0.1:1", "198.51.100.1"),
	} {
		if other == base {
			t.Errorf("%s collides with the base scope %q", name, base)
		}
	}
}

func TestIdempotencyCallerKeyer_ImpersonationHasItsOwnScope(t *testing.T) {
	t.Parallel()
	imp := scopeClaims(scopeTenantA, scopePerson1)
	imp.Act = &jwt.ActClaim{Sub: scopePerson1, SessionID: "0190a000-0000-7000-8000-0000000000a1"}
	k := authn.IdempotencyCallerKeyer(tokenVerifier{
		"plain": scopeClaims(scopeTenantA, scopePerson1),
		"imp":   imp,
	})
	if a, b := scopeOf(t, k, "POST", "/x", "plain", "", ""), scopeOf(t, k, "POST", "/x", "imp", "", ""); a == b {
		t.Fatalf("an impersonation session shares the operator's own scope: %q", a)
	}
}

func TestIdempotencyCallerKeyer_UnverifiedTokenNeverBorrowsATenant(t *testing.T) {
	t.Parallel()
	k := authn.IdempotencyCallerKeyer(tokenVerifier{"good": scopeClaims(scopeTenantA, scopePerson1)})
	// An anonymous caller has no scope: the middleware ignores X-Command-Id and
	// stores nothing (ADR 0068 D11), whatever the client address.
	for name, bearer := range map[string]string{
		"a token the verifier rejects": "forged-token-claiming-tenant-a",
		"no token":                     "",
	} {
		if got := scopeOf(t, k, "POST", "/api/v1/leads", bearer, "127.0.0.1:1", "198.51.100.1"); got != idempotency.Ineligible {
			t.Errorf("%s: scope %q, want Ineligible", name, got)
		}
	}
	// A verified token with no tenant claim is not a tenant scope either.
	k2 := authn.IdempotencyCallerKeyer(tokenVerifier{"t": scopeClaims("", scopePerson1)})
	if got := scopeOf(t, k2, "POST", "/api/v1/leads", "t", "", "198.51.100.1"); got != idempotency.Ineligible {
		t.Fatalf("tenantless token scope %q, want Ineligible", got)
	}
}

// Credential routes never store: a login answer carries a plaintext refresh token
// and the request body hash is of a password. Even a fully verified caller is
// ineligible there.
func TestIdempotencyCallerKeyer_AuthRoutesAreIneligibleEvenWhenSignedIn(t *testing.T) {
	t.Parallel()
	k := authn.IdempotencyCallerKeyer(tokenVerifier{"a1": scopeClaims(scopeTenantA, scopePerson1)})
	for _, path := range []string{
		"/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/logout", "/api/v1/auth/change-password",
		"/api/v1/auth/reset-password", "/api/v1/auth/request-password-reset", "/api/v1/auth/sessions/abc/revoke",
	} {
		if got := scopeOf(t, k, "POST", path, "a1", "127.0.0.1:1", ""); got != idempotency.Ineligible {
			t.Errorf("%s: scope %q, want Ineligible", path, got)
		}
	}
	if got := scopeOf(t, k, "POST", "/api/v1/authors", "a1", "127.0.0.1:1", ""); got == idempotency.Ineligible {
		t.Error("a path that merely starts with /api/v1/auth must not be excluded")
	}
}

// The scope is verified tenant + PERSON + MEMBERSHIP + ...: one person holding two
// memberships (two tokens, same sub and tenant, different membership) must not
// share a scope. Dropping the membership from the hash would merge them.
func TestIdempotencyCallerKeyer_TwoMembershipsOfOnePersonHaveTheirOwnScopes(t *testing.T) {
	t.Parallel()
	m1 := scopeClaims(scopeTenantA, scopePerson1)
	m1.MembershipID = "0190a000-0000-7000-8000-0000000000m1"
	m2 := scopeClaims(scopeTenantA, scopePerson1)
	m2.MembershipID = "0190a000-0000-7000-8000-0000000000m2"
	k := authn.IdempotencyCallerKeyer(tokenVerifier{"m1": m1, "m2": m2})
	a, b := scopeOf(t, k, "POST", "/api/v1/leads", "m1", "", ""), scopeOf(t, k, "POST", "/api/v1/leads", "m2", "", "")
	if a == b {
		t.Fatalf("two memberships of one person share the scope %q", a)
	}
}

func scopeOperatorClaims(homeTenant, person string) *jwt.Claims {
	c := scopeClaims(homeTenant, person)
	c.IsPlatform = true
	c.TenantSlug = authn.PlatformTenantSlug
	return c
}

func scopeWithTenantHeader(t *testing.T, k idempotency.CallerKeyer, bearer string, header ...string) string {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/v1/leads", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	for _, h := range header {
		r.Header.Add("X-Tenant-Id", h)
	}
	return k(r)
}

// The same operator acting in tenant A and then tenant B with the same key, body
// and path must run twice: the acted-on tenant is part of the scope.
func TestIdempotencyCallerKeyer_AnOperatorsActedOnTenantIsPartOfTheScope(t *testing.T) {
	t.Parallel()
	home := "0190a000-0000-7000-8000-0000000000f0"
	k := authn.IdempotencyCallerKeyer(tokenVerifier{
		"op":    scopeOperatorClaims(home, scopePerson1),
		"plain": scopeClaims(scopeTenantA, scopePerson2),
	})
	none := scopeWithTenantHeader(t, k, "op")
	inA := scopeWithTenantHeader(t, k, "op", scopeTenantA)
	inB := scopeWithTenantHeader(t, k, "op", scopeTenantB)
	if inA == inB || inA == none || inB == none {
		t.Fatalf("scopes must differ: none=%q A=%q B=%q", none, inA, inB)
	}
	// Canonicalised with the scope middleware's own function: case does not matter.
	if up := scopeWithTenantHeader(t, k, "op", strings.ToUpper(scopeTenantA)); up != inA {
		t.Fatalf("upper-case spelling of the same tenant got another scope: %q != %q", up, inA)
	}
	// Acting in the operator's own home tenant is the same as not naming one.
	if own := scopeWithTenantHeader(t, k, "op", home); own != none {
		t.Fatalf("naming the home tenant changed the scope: %q != %q", own, none)
	}
	// Blank values are absent, exactly as the scope middleware reads them.
	if blank := scopeWithTenantHeader(t, k, "op", "  "); blank != none {
		t.Fatalf("a blank header changed the scope: %q != %q", blank, none)
	}
}

// A value the scope middleware will refuse must not mint distinct scopes: every
// malformed value shares one (and ends in a 4xx that is never stored); the same
// goes for several values.
func TestIdempotencyCallerKeyer_RefusedTenantHeadersShareOneScope(t *testing.T) {
	t.Parallel()
	k := authn.IdempotencyCallerKeyer(tokenVerifier{"op": scopeOperatorClaims("0190a000-0000-7000-8000-0000000000f0", scopePerson1)})
	a := scopeWithTenantHeader(t, k, "op", "not-a-uuid")
	b := scopeWithTenantHeader(t, k, "op", "also not a uuid")
	if a != b {
		t.Fatalf("two malformed values minted distinct scopes %q and %q", a, b)
	}
	m1 := scopeWithTenantHeader(t, k, "op", scopeTenantA, scopeTenantB)
	m2 := scopeWithTenantHeader(t, k, "op", scopeTenantB, scopeTenantA)
	if m1 != m2 || m1 == a {
		t.Fatalf("repeated headers: %q %q (malformed %q)", m1, m2, a)
	}
}

// For everyone but a platform operator the scope middleware ignores X-Tenant-Id, so
// it must not be able to change the scope: varying it per attempt would otherwise
// defeat replay protection and run the same command twice.
func TestIdempotencyCallerKeyer_TenantHeaderIsIgnoredForNonOperators(t *testing.T) {
	t.Parallel()
	imp := scopeOperatorClaims("0190a000-0000-7000-8000-0000000000f0", scopePerson1)
	imp.Act = &jwt.ActClaim{Sub: scopePerson1, SessionID: "0190a000-0000-7000-8000-0000000000a1"}
	k := authn.IdempotencyCallerKeyer(tokenVerifier{
		"plain": scopeClaims(scopeTenantA, scopePerson2),
		"imp":   imp,
	})
	for _, bearer := range []string{"plain", "imp"} {
		base := scopeWithTenantHeader(t, k, bearer)
		for _, h := range [][]string{{scopeTenantB}, {"junk"}, {scopeTenantA, scopeTenantB}} {
			if got := scopeWithTenantHeader(t, k, bearer, h...); got != base {
				t.Errorf("%s: header %v changed the scope: %q != %q", bearer, h, got, base)
			}
		}
	}
}

// Two tenants behind the same BFF (same peer, same client address) that pick
// the same X-Command-Id must each execute; neither may be handed the other's
// stored response.
func TestIdempotencyMiddleware_DoesNotReplayAcrossTenantsOrRoutes(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"owner":"` + r.Header.Get("Authorization") + `"}`))
	})
	keyer := authn.IdempotencyCallerKeyer(tokenVerifier{
		"a": scopeClaims(scopeTenantA, scopePerson1),
		"b": scopeClaims(scopeTenantB, scopePerson2),
	})
	h := idempotency.Middleware(idempotency.NewInMemoryStore(nil), nil, 0, keyer, slog.New(slog.DiscardHandler))(next)
	key := uuid.NewString()
	post := func(path, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), "POST", path, strings.NewReader(`{"n":1}`))
		r.RemoteAddr = "127.0.0.1:9"
		r.Header.Set("X-Forwarded-For", "198.51.100.1")
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set(idempotency.CommandIDHeader, key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := post("/api/v1/leads", "a"); rec.Code != http.StatusCreated {
		t.Fatalf("tenant A: %d", rec.Code)
	}
	if rec := post("/api/v1/leads", "a"); rec.Header().Get(idempotency.ReplayHeader) != "true" {
		t.Fatal("same caller, same route, same key must replay")
	}
	rec := post("/api/v1/leads", "b")
	if rec.Header().Get(idempotency.ReplayHeader) != "" || !strings.Contains(rec.Body.String(), "Bearer b") {
		t.Fatalf("tenant B was served tenant A's stored response: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/api/v1/orders", "a"); rec.Header().Get(idempotency.ReplayHeader) != "" {
		t.Fatal("the same key on another route must not replay the first route's response")
	}
	if got := runs.Load(); got != 3 {
		t.Fatalf("handler ran %d times, want 3 (A, B, A on another route)", got)
	}
}
