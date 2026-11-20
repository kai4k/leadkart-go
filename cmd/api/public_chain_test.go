package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
)

// These tests drive the chain through buildPublicChain, the constructor main()
// itself calls, so each wiring mistake that used to survive (a dropped option, two
// routes' limits swapped, the route limiter moved behind idempotency) fails here.

type chainVerifier map[string]*jwt.Claims

func (v chainVerifier) Verify(token string) (*jwt.Claims, error) {
	if c, ok := v[token]; ok {
		return c, nil
	}
	return nil, errors.New("bad token")
}

func chainClaims(tenant, person string) *jwt.Claims {
	c := &jwt.Claims{TenantID: tenant, MembershipID: person + "-m"}
	c.Subject = person
	return c
}

// distinctBursts gives every credential route its own burst and a refill so slow
// it never matters inside a test, so the number of requests a route admits is
// exactly its configured burst.
func distinctBursts() config.RateLimitConfig {
	slow := func(burst int) config.RouteLimit { return config.RouteLimit{PerMinute: 0.0001, Burst: burst} }
	return config.RateLimitConfig{
		Login:                slow(2),
		Refresh:              slow(3),
		PasswordResetRequest: slow(4),
		PasswordReset:        slow(5),
		EmailChangeConfirm:   slow(6),
		TenantRegistration:   slow(7),
	}
}

type builtChain struct {
	handler http.Handler
	runs    *atomic.Int32
}

func newChain(t *testing.T, trusted string, v chainVerifier) builtChain {
	t.Helper()
	cfg := config.Defaults()
	cfg.HTTP.TrustedProxies = trusted
	cfg.RateLimit = distinctBursts()
	mw, err := buildPublicChain(cfg, silentLogger(), idempotency.NewInMemoryStore(nil), v, time.Now)
	if err != nil {
		t.Fatalf("buildPublicChain: %v", err)
	}
	runs := &atomic.Int32{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		runs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	return builtChain{handler: mw(inner), runs: runs}
}

func (c builtChain) do(t *testing.T, method, path, remote, xff, bearer, commandID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(`{"n":1}`))
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if commandID != "" {
		r.Header.Set(idempotency.CommandIDHeader, commandID)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, r)
	return w
}

// A route admits exactly its configured burst per client, and the first refusal
// is 429 with Retry-After. A swapped pair of routes, a dropped RouteRateLimits
// option or a changed per-route number changes the count.
func TestPublicChain_EachCredentialRouteAdmitsExactlyItsOwnBurst(t *testing.T) {
	t.Parallel()
	want := map[string]int{
		"/api/v1/auth/login":                  2,
		"/api/v1/auth/refresh":                3,
		"/api/v1/auth/request-password-reset": 4,
		"/api/v1/auth/reset-password":         5,
		"/api/v1/auth/confirm-email-change":   6,
		"/api/v1/tenants":                     7,
	}
	for path, burst := range want {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			c := newChain(t, "none", nil)
			for i := 1; i <= burst; i++ {
				if w := c.do(t, http.MethodPost, path, "203.0.113.5:4000", "", "", ""); w.Code != http.StatusOK {
					t.Fatalf("request %d of %d = %d, want 200", i, burst, w.Code)
				}
			}
			w := c.do(t, http.MethodPost, path, "203.0.113.5:4000", "", "", "")
			if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
				t.Fatalf("request %d = %d (Retry-After %q), want 429 with Retry-After", burst+1, w.Code, w.Header().Get("Retry-After"))
			}
			// Another client is unaffected: the bucket is per client.
			if w := c.do(t, http.MethodPost, path, "203.0.113.6:4000", "", "", ""); w.Code != http.StatusOK {
				t.Fatalf("another client = %d, want 200", w.Code)
			}
		})
	}
}

// Behind a trusted BFF, two users are two buckets (the forwarded client), and a
// user cannot reach another's bucket by writing a hop to the left.
func TestPublicChain_TheTrustedProxyListIsWired(t *testing.T) {
	t.Parallel()
	const login = "/api/v1/auth/login"
	c := newChain(t, "10.0.0.0/8", nil)
	for i := range 2 {
		if w := c.do(t, http.MethodPost, login, "10.0.0.5:4000", "198.51.100.1", "", ""); w.Code != http.StatusOK {
			t.Fatalf("user A request %d = %d", i+1, w.Code)
		}
	}
	if w := c.do(t, http.MethodPost, login, "10.0.0.5:4000", "198.51.100.1", "", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("user A over the burst = %d, want 429", w.Code)
	}
	if w := c.do(t, http.MethodPost, login, "10.0.0.5:4000", "198.51.100.2", "", ""); w.Code != http.StatusOK {
		t.Fatalf("user B behind the same BFF = %d, want 200 (own bucket)", w.Code)
	}
	// A hop the client wrote itself, left of the one the BFF appended, moves nothing.
	if w := c.do(t, http.MethodPost, login, "10.0.0.5:4000", "garbage, 198.51.100.1", "", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("user A with junk written to the left = %d, want 429 (still A's bucket)", w.Code)
	}
	// An untrusted peer's header is not believed.
	for i := range 2 {
		if w := c.do(t, http.MethodPost, login, "203.0.113.9:4000", "198.51.100.77", "", ""); w.Code != http.StatusOK {
			t.Fatalf("untrusted peer request %d = %d", i+1, w.Code)
		}
	}
	if w := c.do(t, http.MethodPost, login, "203.0.113.9:4000", "198.51.100.78", "", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("untrusted peer rotating its header = %d, want 429 (keyed by its socket address)", w.Code)
	}
}

// A BFF that forgets the header puts everyone in the peer's bucket: the chain
// still answers (it must not fail open), and the limiter keys the peer.
func TestPublicChain_ATrustedPeerWithoutAHeaderIsKeyedByThePeer(t *testing.T) {
	t.Parallel()
	c := newChain(t, "10.0.0.0/8", nil)
	for range 2 {
		c.do(t, http.MethodPost, "/api/v1/auth/login", "10.0.0.5:4000", "", "", "")
	}
	if w := c.do(t, http.MethodPost, "/api/v1/auth/login", "10.0.0.5:4000", "", "", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("= %d, want 429 on the shared peer bucket", w.Code)
	}
}

// The route limiter sits BEFORE idempotency: a request the limiter refuses is
// never answered from a stored response. With the order reversed, replays of a
// stored 2xx would be served past the limit.
func TestPublicChain_TheRouteLimiterRunsBeforeIdempotency(t *testing.T) {
	t.Parallel()
	v := chainVerifier{"a": chainClaims("tenant-a", "person-a")}
	c := newChain(t, "none", v)
	key := uuid.NewString()
	const path = "/api/v1/tenants" // burst 7
	var codes []int
	for range 9 {
		codes = append(codes, c.do(t, http.MethodPost, path, "203.0.113.5:4000", "", "a", key).Code)
	}
	for i, code := range codes {
		want := http.StatusOK
		if i >= 7 {
			want = http.StatusTooManyRequests
		}
		if code != want {
			t.Fatalf("request %d = %d, want %d (all: %v)", i+1, code, want, codes)
		}
	}
	if got := c.runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1 (the first execution; the rest are replays or refusals)", got)
	}
}

// The idempotency scope is the verified caller: two tenants using the same key,
// body and path each execute. With the default (anonymous, per client) keyer the
// second would be handed the first's response.
func TestPublicChain_TheIdempotencyScopeIsTheVerifiedCaller(t *testing.T) {
	t.Parallel()
	v := chainVerifier{
		"a": chainClaims("tenant-a", "person-a"),
		"b": chainClaims("tenant-b", "person-b"),
	}
	c := newChain(t, "none", v)
	key := uuid.NewString()
	for _, bearer := range []string{"a", "b"} {
		w := c.do(t, http.MethodPost, "/api/v1/leads", "203.0.113.5:4000", "", bearer, key)
		if w.Code != http.StatusOK || w.Header().Get(idempotency.ReplayHeader) != "" {
			t.Fatalf("caller %s = %d replay=%q, want a fresh execution", bearer, w.Code, w.Header().Get(idempotency.ReplayHeader))
		}
	}
	if got := c.runs.Load(); got != 2 {
		t.Fatalf("handler ran %d times, want 2", got)
	}
	if w := c.do(t, http.MethodPost, "/api/v1/leads", "203.0.113.5:4000", "", "a", key); w.Header().Get(idempotency.ReplayHeader) != "true" {
		t.Fatalf("the same caller repeating the key must replay, got %d", w.Code)
	}
}

func TestBuildPublicChain_RefusesAnUnparseableTrustedProxyList(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.HTTP.TrustedProxies = "not-an-ip"
	if _, err := buildPublicChain(cfg, silentLogger(), idempotency.NewInMemoryStore(nil), chainVerifier{}, time.Now); err == nil {
		t.Fatal("want an error")
	}
}
