package httpmw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

func TestRateLimiter_AllowsUpToBurst_Then429(t *testing.T) {
	t.Parallel()
	rl := httpmw.NewRateLimiter(httpmw.LimiterConfig{
		RatePerSecond: 1, // refill 1 per second; bucket effectively static for the test
		Burst:         3,
	}, httpmw.IPLimiterKeyer)
	mw := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := range 3 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:5555"
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("burst %d: got %d want 200", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-burst: got %d want 429", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"rate_limited"`) {
		t.Fatalf("body missing rate_limited code: %q", body)
	}
}

func TestRateLimiter_DistinctIPs_HaveSeparateBuckets(t *testing.T) {
	t.Parallel()
	rl := httpmw.NewRateLimiter(httpmw.LimiterConfig{
		RatePerSecond: 1,
		Burst:         1,
	}, httpmw.IPLimiterKeyer)
	mw := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Two distinct IPs each get their own bucket — both should pass on
	// the first hit, both should 429 on the second.
	addrs := []string{"10.0.0.1:1111", "10.0.0.2:2222"}
	for _, addr := range addrs {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("first hit from %s: got %d want 200", addr, rec.Code)
		}
	}
	for _, addr := range addrs {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		req.RemoteAddr = addr
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("second hit from %s: got %d want 429", addr, rec.Code)
		}
	}
}

func TestRateLimiter_EmptyKey_Skips(t *testing.T) {
	t.Parallel()
	// A keyer that returns empty disables the limiter for that
	// request — useful for healthchecks or trusted internal traffic.
	skipKeyer := func(*http.Request) httpmw.LimiterKey { return "" }
	rl := httpmw.NewRateLimiter(httpmw.LimiterConfig{
		RatePerSecond: 1,
		Burst:         1,
	}, skipKeyer)
	mw := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := range 5 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1111"
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("hit %d with empty key: got %d want 200 (limiter should skip)", i, rec.Code)
		}
	}
}

func TestRateLimiter_PanicsOnInvalidConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func()
	}{
		{"zero rate", func() {
			httpmw.NewRateLimiter(httpmw.LimiterConfig{RatePerSecond: 0, Burst: 1}, httpmw.IPLimiterKeyer)
		}},
		{"zero burst", func() {
			httpmw.NewRateLimiter(httpmw.LimiterConfig{RatePerSecond: 1, Burst: 0}, httpmw.IPLimiterKeyer)
		}},
		{"nil keyer", func() {
			httpmw.NewRateLimiter(httpmw.LimiterConfig{RatePerSecond: 1, Burst: 1}, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic on invalid config")
				}
			}()
			tc.fn()
		})
	}
}
