package httpmw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

func routeLimitedHandler(t *testing.T, trustedCSV string) http.Handler {
	t.Helper()
	trusted, err := httpmw.ParseTrustedProxies(trustedCSV)
	if err != nil {
		t.Fatal(err)
	}
	mw := httpmw.RouteRateLimit(
		httpmw.RouteRateLimits{"POST /login": {RatePerSecond: 0.001, Burst: 2}},
		httpmw.ClientIPKeyer(trusted),
	)
	return mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
}

func do(ctx context.Context, h http.Handler, method, path, remote, xff string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, method, path, nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRouteRateLimit_LimitsOnlyTheListedRoutePerClient(t *testing.T) {
	t.Parallel()
	h := routeLimitedHandler(t, "")
	for i := range 2 {
		if rec := do(t.Context(), h, "POST", "/login", "192.0.2.1:1", ""); rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: %d, want 200", i+1, rec.Code)
		}
	}
	rec := do(t.Context(), h, "POST", "/login", "192.0.2.1:1", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third attempt: %d, want 429", rec.Code)
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer", rec.Header().Get("Retry-After"))
	}
	if rec := do(t.Context(), h, "POST", "/login", "192.0.2.2:1", ""); rec.Code != http.StatusOK {
		t.Fatalf("other client: %d, want 200", rec.Code)
	}
	for _, c := range []struct{ method, path string }{{"GET", "/login"}, {"POST", "/other"}, {"POST", "/login/"}} {
		for range 5 {
			if rec := do(t.Context(), h, c.method, c.path, "192.0.2.1:1", ""); rec.Code != http.StatusOK {
				t.Fatalf("%s %s limited: %d", c.method, c.path, rec.Code)
			}
		}
	}
}

func TestRouteRateLimit_BucketsFollowTheForwardedClientOnlyFromATrustedPeer(t *testing.T) {
	t.Parallel()
	h := routeLimitedHandler(t, "127.0.0.1")
	// Two end clients behind the trusted BFF do not share a bucket.
	for range 2 {
		do(t.Context(), h, "POST", "/login", "127.0.0.1:9", "198.51.100.1")
	}
	if rec := do(t.Context(), h, "POST", "/login", "127.0.0.1:9", "198.51.100.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("same forwarded client: %d, want 429", rec.Code)
	}
	if rec := do(t.Context(), h, "POST", "/login", "127.0.0.1:9", "198.51.100.2"); rec.Code != http.StatusOK {
		t.Fatalf("other forwarded client: %d, want 200", rec.Code)
	}
	// An untrusted peer cannot pick its own bucket by rotating the header.
	for i := range 2 {
		do(t.Context(), h, "POST", "/login", "203.0.113.9:9", "198.51.100."+strconv.Itoa(10+i))
	}
	if rec := do(t.Context(), h, "POST", "/login", "203.0.113.9:9", "198.51.100.99"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("header rotation from an untrusted peer: %d, want 429", rec.Code)
	}
}
