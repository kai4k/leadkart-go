package httpmw_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/common/idempotency"
)

func publicChainFor(t *testing.T, trustedCSV string, routes httpmw.RouteRateLimits) http.Handler {
	t.Helper()
	trusted, err := httpmw.ParseTrustedProxies(trustedCSV)
	if err != nil {
		t.Fatal(err)
	}
	chain := httpmw.PublicChain(httpmw.PublicChainConfig{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		IdempotencyStore: idempotency.NewInMemoryStore(nil),
		Now:              time.Now,
		IPRateLimit:      httpmw.LimiterConfig{RatePerSecond: 0.001, Burst: 3},
		TrustedProxies:   trusted,
		RouteRateLimits:  routes,
	})
	return chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
}

// Behind the BFF every request has the BFF as socket peer; the global bucket
// must follow the forwarded client, not the proxy.
func TestPublicChain_GlobalLimiterFollowsForwardedClientFromTrustedPeer(t *testing.T) {
	t.Parallel()
	h := publicChainFor(t, "127.0.0.1", nil)
	for range 3 {
		do(t.Context(), h, "GET", "/x", "127.0.0.1:9", "198.51.100.1")
	}
	if rec := do(t.Context(), h, "GET", "/x", "127.0.0.1:9", "198.51.100.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted client: %d, want 429", rec.Code)
	}
	if rec := do(t.Context(), h, "GET", "/x", "127.0.0.1:9", "198.51.100.2"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("a second client behind the same proxy shares the first one's bucket")
	}
}

func TestPublicChain_NoTrustedProxiesIgnoresForwardedFor(t *testing.T) {
	t.Parallel()
	h := publicChainFor(t, "", nil)
	for i := range 3 {
		do(t.Context(), h, "GET", "/x", "203.0.113.9:9", "198.51.100."+string(rune('1'+i)))
	}
	if rec := do(t.Context(), h, "GET", "/x", "203.0.113.9:9", "198.51.100.77"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rotating X-Forwarded-For escaped the limit: %d", rec.Code)
	}
}

func TestPublicChain_RouteLimitIsTighterThanTheGlobalOne(t *testing.T) {
	t.Parallel()
	h := publicChainFor(t, "127.0.0.1", httpmw.RouteRateLimits{"POST /login": {RatePerSecond: 0.001, Burst: 1}})
	if rec := do(t.Context(), h, "POST", "/login", "127.0.0.1:9", "198.51.100.1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("first: %d, want the handler's 401", rec.Code)
	}
	if rec := do(t.Context(), h, "POST", "/login", "127.0.0.1:9", "198.51.100.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second: %d, want 429 from the route limit", rec.Code)
	}
}
