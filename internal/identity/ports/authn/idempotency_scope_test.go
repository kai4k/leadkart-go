package authn_test

import (
	"io"
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

func clientFromXFF(r *http.Request) string { return r.Header.Get("X-Forwarded-For") }

func TestIdempotencyCallerKeyer_ScopesByVerifiedTenantPersonAndRoute(t *testing.T) {
	t.Parallel()
	k := authn.IdempotencyCallerKeyer(tokenVerifier{
		"a1":  scopeClaims(scopeTenantA, scopePerson1),
		"a1b": scopeClaims(scopeTenantA, scopePerson1), // same caller, newer token
		"a2":  scopeClaims(scopeTenantA, scopePerson2),
		"b1":  scopeClaims(scopeTenantB, scopePerson1),
	}, clientFromXFF)

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
	}, clientFromXFF)
	if a, b := scopeOf(t, k, "POST", "/x", "plain", "", ""), scopeOf(t, k, "POST", "/x", "imp", "", ""); a == b {
		t.Fatalf("an impersonation session shares the operator's own scope: %q", a)
	}
}

func TestIdempotencyCallerKeyer_UnverifiedTokenNeverBorrowsATenant(t *testing.T) {
	t.Parallel()
	k := authn.IdempotencyCallerKeyer(tokenVerifier{"good": scopeClaims(scopeTenantA, scopePerson1)}, clientFromXFF)
	got := scopeOf(t, k, "POST", "/x", "forged-token-claiming-tenant-a", "127.0.0.1:1", "198.51.100.1")
	if strings.HasPrefix(got, "tenant:") {
		t.Fatalf("an unverifiable token got tenant scope %q", got)
	}
	if !strings.HasPrefix(got, "anon:198.51.100.1:") {
		t.Fatalf("scope %q, want the anonymous namespace keyed by the client address", got)
	}
	if blank := scopeOf(t, k, "POST", "/x", "", "127.0.0.1:1", ""); blank != "" {
		t.Fatalf("no client address and no token must give an empty scope (400), got %q", blank)
	}
	// A verified token with no tenant claim is not a tenant scope either.
	k2 := authn.IdempotencyCallerKeyer(tokenVerifier{"t": scopeClaims("", scopePerson1)}, clientFromXFF)
	if got := scopeOf(t, k2, "POST", "/x", "t", "", "198.51.100.1"); !strings.HasPrefix(got, "anon:") {
		t.Fatalf("tenantless token scope %q, want anonymous", got)
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
	}, clientFromXFF)
	h := idempotency.Middleware(idempotency.NewInMemoryStore(nil), nil, 0, keyer)(next)
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
