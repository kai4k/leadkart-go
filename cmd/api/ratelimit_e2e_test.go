//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// The real chain (trusted-proxy keyer, per-route limit, Postgres idempotency
// store) around a stub handler; the test client is the "BFF" (socket peer
// 127.0.0.1) and names the end client in X-Forwarded-For.
func TestE2E_RateLimit_PerRouteKeyedByForwardedClient(t *testing.T) {
	f := newE2EFixture(t)
	trusted, err := httpmw.ParseTrustedProxies("127.0.0.1/32,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	chain := httpmw.PublicChain(httpmw.PublicChainConfig{
		Logger: silentLogger(), IdempotencyStore: idempotency.NewPostgresStore(f.Pool), Now: time.Now,
		IPRateLimit:     httpmw.LimiterConfig{RatePerSecond: 100, Burst: 100},
		TrustedProxies:  trusted,
		RouteRateLimits: httpmw.RouteRateLimits{"POST /api/v1/auth/login": {RatePerSecond: 0.001, Burst: 3}},
	})
	srv := httptest.NewServer(chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })))
	t.Cleanup(srv.Close)
	post := func(client string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/auth/login", strings.NewReader(`{}`))
		req.Header.Set("X-Forwarded-For", client)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	for i := range 3 {
		if got := post("198.51.100.1"); got == http.StatusTooManyRequests {
			t.Fatalf("attempt %d limited early", i+1)
		}
	}
	if got := post("198.51.100.1"); got != http.StatusTooManyRequests {
		t.Fatalf("4th from same client: %d, want 429", got)
	}
	if got := post("198.51.100.2"); got == http.StatusTooManyRequests {
		t.Fatal("different client behind the same BFF must have its own bucket")
	}
}

// Two tenants behind the same BFF and client address that choose the same
// X-Command-Id each run their command; neither replays the other's stored
// response, through the real Issuer and the Postgres store.
func TestE2E_Idempotency_ScopedPerVerifiedTenantNotPerBFF(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	trusted, err := httpmw.ParseTrustedProxies("127.0.0.1/32,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	chain := httpmw.PublicChain(httpmw.PublicChainConfig{
		Logger: silentLogger(), IdempotencyStore: idempotency.NewPostgresStore(f.Pool), Now: time.Now,
		IPRateLimit:      httpmw.LimiterConfig{RatePerSecond: 100, Burst: 100},
		TrustedProxies:   trusted,
		IdempotencyKeyer: authn.IdempotencyCallerKeyer(f.Issuer),
	})
	srv := httptest.NewServer(chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"tenant":"` + r.Header.Get("X-Probe") + `"}`))
	})))
	t.Cleanup(srv.Close)

	token := func(tenantID string) string {
		tok, err := f.Issuer.Issue(jwt.IssueArgs{
			PersonID: ids.NewV7().String(), TenantID: tenantID, MembershipID: ids.NewV7().String(),
			SecurityStamp: ids.NewV7().String(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	tokA, tokB := token(ids.NewV7().String()), token(ids.NewV7().String())
	cmdID := uuid.NewString()
	post := func(tok, probe string) (*http.Response, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/leads", strings.NewReader(`{"n":1}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		req.Header.Set("X-Probe", probe)
		req.Header.Set(idempotency.CommandIDHeader, cmdID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, _ := post(tokA, "A"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("tenant A: %d", resp.StatusCode)
	}
	if resp, _ := post(tokA, "A"); resp.Header.Get(idempotency.ReplayHeader) != "true" {
		t.Fatal("the same caller retrying the same command must replay")
	}
	resp, body := post(tokB, "B")
	if resp.Header.Get(idempotency.ReplayHeader) != "" || !strings.Contains(body, `"B"`) {
		t.Fatalf("tenant B was served tenant A's stored response: %d %s", resp.StatusCode, body)
	}
	if runs.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", runs.Load())
	}
}
