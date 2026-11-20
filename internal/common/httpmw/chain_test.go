package httpmw_test

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
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

// Fix round 12, item 3(c): one IPv6 /48 is 65 536 /64s, each with its own full
// bucket. The credential routes therefore also count by /48 at four times the per-/64
// rate, so a site that rotates /64s hits a ceiling; IPv4 is untouched.
func TestPublicChain_RouteLimitsAlsoCountPerIPv6Block(t *testing.T) {
	t.Parallel()
	routes := httpmw.RouteRateLimits{"POST /login": {RatePerSecond: 0.001, Burst: 2}} // 2 per /64, 8 per /48
	h := publicChainFor(t, "127.0.0.1", routes)
	const peer = "127.0.0.1:9"

	// Many distinct /64s of ONE /48: each is far below its own limit, the /48 is not.
	allowed := 0
	for i := range 30 {
		xff := "2001:db8:1:" + strconv.FormatInt(int64(i)+1, 16) + "::1"
		if rec := do(t.Context(), h, "POST", "/login", peer, xff); rec.Code != http.StatusTooManyRequests {
			allowed++
		}
	}
	if allowed != 2*httpmw.IPv6BlockRateMultiplier {
		t.Fatalf("a /48 got %d requests through across its /64s, want exactly %d (4x the per-/64 burst of 2)", allowed, 2*httpmw.IPv6BlockRateMultiplier)
	}
	// A different /48 is a different bucket.
	if rec := do(t.Context(), h, "POST", "/login", peer, "2001:db8:2:1::1"); rec.Code == http.StatusTooManyRequests {
		t.Fatalf("another /48 was limited: %d", rec.Code)
	}
	// IPv4 is limited by its own bucket only (burst 2), not by any block.
	for i := range 2 {
		if rec := do(t.Context(), h, "POST", "/login", peer, "198.51.100."+strconv.Itoa(10+i)); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("distinct IPv4 clients share a bucket: %d", rec.Code)
		}
	}
	for range 2 {
		do(t.Context(), h, "POST", "/login", peer, "198.51.100.77")
	}
	if rec := do(t.Context(), h, "POST", "/login", peer, "198.51.100.77"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("an IPv4 client's own bucket did not limit: %d", rec.Code)
	}
}
