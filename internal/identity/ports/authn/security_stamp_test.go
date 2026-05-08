package authn_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// fakeStampValidator is the consumer-side fake for [authn.StampValidator].
// Production wires *adapters.SecurityStampValidator (which fronts the
// HybridCache + Postgres read-through). Tests substitute this so they
// don't need miniredis just to assert middleware composition.
type fakeStampValidator struct {
	wantPersonID string
	wantStamp    string
	fresh        bool
	err          error
	calls        atomic.Int64
}

func (f *fakeStampValidator) IsFresh(_ context.Context, personID, claimStamp string) (bool, error) {
	f.calls.Add(1)
	if f.err != nil {
		return false, f.err
	}
	if f.wantPersonID != "" && personID != f.wantPersonID {
		return false, nil
	}
	if f.wantStamp != "" && claimStamp != f.wantStamp {
		return false, nil
	}
	return f.fresh, nil
}

const (
	stampPersonID = "01999999-aaaa-7000-8000-aaaaaaaaaaaa"
	stampFresh    = "00000000-0000-7000-8000-000000000001"
	stampStale    = "00000000-0000-7000-8000-000000000099"
)

// freshClaims returns a Claims with sub + security_stamp + tenant_id
// populated — the shape every production JWT carries.
func freshClaims(stamp string) *jwt.Claims {
	c := &jwt.Claims{
		TenantID:      "tenant-test",
		SecurityStamp: stamp,
	}
	c.Subject = stampPersonID
	return c
}

func TestRequireFreshStamp_FreshStamp_CallsNext(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{wantToken: "tok", claims: freshClaims(stampFresh)}
	stamp := &fakeStampValidator{wantPersonID: stampPersonID, wantStamp: stampFresh, fresh: true}
	s := &sentinel{}

	mw := authn.RequireFreshStamp(v, stamp)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !s.called {
		t.Fatal("next did not run on fresh stamp")
	}
	if calls := stamp.calls.Load(); calls != 1 {
		t.Fatalf("validator calls: got %d want 1", calls)
	}
}

func TestRequireFreshStamp_StaleStamp_Returns401StaleToken(t *testing.T) {
	t.Parallel()
	// Token still carries stampFresh, but the validator (= source of
	// truth) reports false → token was issued before a credential
	// rotation that has since happened. SPA should re-login.
	v := &fakeVerifier{wantToken: "tok", claims: freshClaims(stampFresh)}
	stamp := &fakeStampValidator{fresh: false}
	s := &sentinel{}

	mw := authn.RequireFreshStamp(v, stamp)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if s.called {
		t.Fatal("next ran despite stale stamp")
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"stale_token"`) {
		t.Fatalf("body: got %q want stale_token error code", string(body))
	}
	// Per RFC 6750 §3 — every 401 must carry the WWW-Authenticate
	// Bearer challenge (inherited from RequireAuth's writeError).
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate: got %q want Bearer challenge", got)
	}
}

func TestRequireFreshStamp_ValidatorError_Returns401Unauthenticated(t *testing.T) {
	t.Parallel()
	// Repo / cache transport failure. Per security.md "fail closed":
	// generic 401 with the same code as a verify failure so an attacker
	// can't probe internal-error vs. real-stale via response
	// differentiation.
	v := &fakeVerifier{wantToken: "tok", claims: freshClaims(stampFresh)}
	stamp := &fakeStampValidator{err: errors.New("postgres: connection refused")}
	s := &sentinel{}

	mw := authn.RequireFreshStamp(v, stamp)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if s.called {
		t.Fatal("next ran despite validator error")
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"unauthenticated"`) {
		t.Fatalf("body: got %q want unauthenticated error code", string(body))
	}
	if strings.Contains(string(body), "stale_token") {
		t.Fatalf("body: got %q — must not surface stale_token on internal error (security.md fail-closed)", string(body))
	}
}

func TestRequireFreshStamp_NoBearer_Returns401(t *testing.T) {
	t.Parallel()
	// Missing Authorization header → RequireAuth short-circuits before
	// the stamp check ever runs. Validator must NOT be called.
	v := &fakeVerifier{}
	stamp := &fakeStampValidator{fresh: true}
	s := &sentinel{}

	mw := authn.RequireFreshStamp(v, stamp)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if s.called {
		t.Fatal("next ran despite missing bearer")
	}
	if calls := stamp.calls.Load(); calls != 0 {
		t.Fatalf("validator calls on missing bearer: got %d want 0 (RequireAuth not gating)", calls)
	}
}

func TestRequireFreshStamp_VerifierFails_Returns401_NoValidatorCall(t *testing.T) {
	t.Parallel()
	// Invalid JWT → RequireAuth short-circuits; stamp validator never
	// queried. Asserts the gating order: signature first, stamp second.
	v := &fakeVerifier{err: jwt.ErrInvalidToken}
	stamp := &fakeStampValidator{fresh: true}
	s := &sentinel{}

	mw := authn.RequireFreshStamp(v, stamp)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer fake.token"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if s.called {
		t.Fatal("next ran despite verify failure")
	}
	if calls := stamp.calls.Load(); calls != 0 {
		t.Fatalf("validator calls on verify-failure: got %d want 0", calls)
	}
}

func TestRequireFreshStamp_EmptyStampClaim_Returns401(t *testing.T) {
	t.Parallel()
	// Defense-in-depth: an attacker who crafts/strips the stamp claim
	// (post-compromise of signing key, RFC 8725 §3.1 mitigation lives
	// in jwt.Verify but this is the second layer) gets 401 without
	// the validator ever running.
	cases := []struct {
		name  string
		stamp string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &fakeVerifier{wantToken: "tok", claims: freshClaims(tc.stamp)}
			stamp := &fakeStampValidator{fresh: true}
			s := &sentinel{}

			mw := authn.RequireFreshStamp(v, stamp)(s.handler())
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d want 401", rec.Code)
			}
			if s.called {
				t.Fatalf("next ran with empty stamp claim (%q)", tc.stamp)
			}
			if calls := stamp.calls.Load(); calls != 0 {
				t.Fatalf("validator calls on empty stamp: got %d want 0", calls)
			}
		})
	}
}

func TestRequireFreshStamp_EmptySubject_Returns401(t *testing.T) {
	t.Parallel()
	// Same shape as empty stamp — JWT-issuance bug guards.
	c := &jwt.Claims{TenantID: "tenant-test", SecurityStamp: stampFresh}
	c.Subject = ""
	v := &fakeVerifier{wantToken: "tok", claims: c}
	stamp := &fakeStampValidator{fresh: true}
	s := &sentinel{}

	mw := authn.RequireFreshStamp(v, stamp)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if s.called {
		t.Fatal("next ran with empty Subject")
	}
}

func TestRequireFreshStamp_PanicsOnNilVerifier(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil verifier (wiring bug)")
		}
	}()
	_ = authn.RequireFreshStamp(nil, &fakeStampValidator{})
}

func TestRequireFreshStamp_PanicsOnNilValidator(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil validator (wiring bug)")
		}
	}()
	_ = authn.RequireFreshStamp(&fakeVerifier{}, nil)
}
