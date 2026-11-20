package httpmw

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stepClock is a settable clock: the sweeps and refills below run on it, so
// nothing here waits on wall time.
type stepClock struct{ t atomic.Int64 }

func newStepClock() *stepClock {
	c := &stepClock{}
	c.t.Store(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *stepClock) now() time.Time          { return time.Unix(0, c.t.Load()).UTC() }
func (c *stepClock) advance(d time.Duration) { c.t.Add(int64(d)) }

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func hit(t *testing.T, h http.Handler, remote string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRateLimiter_IPv6NeighboursInOneSlash64ShareABucket(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	l := newRateLimiter(LimiterConfig{RatePerSecond: 0.001, Burst: 2}, ClientIPKeyer(nil), clk.now)
	h := l.Middleware()(okHandler())
	if w := hit(t, h, "[2001:db8:1:2::1]:80"); w.Code != http.StatusNoContent {
		t.Fatalf("first = %d", w.Code)
	}
	if w := hit(t, h, "[2001:db8:1:2::2]:80"); w.Code != http.StatusNoContent {
		t.Fatalf("second = %d", w.Code)
	}
	// A third address in the SAME /64 finds the bucket empty.
	if w := hit(t, h, "[2001:db8:1:2:dead:beef:0:3]:80"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("third neighbour = %d, want 429: a /64 is one bucket", w.Code)
	}
	// Another /64 has its own.
	if w := hit(t, h, "[2001:db8:1:9::1]:80"); w.Code != http.StatusNoContent {
		t.Fatalf("other /64 = %d, want 204", w.Code)
	}
	if n := len(l.limiters); n != 2 {
		t.Fatalf("%d buckets, want 2 (one per /64)", n)
	}
}

func TestRateLimiter_AnIdleBucketIsSweptAndRecreatedFresh(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	l := newRateLimiter(LimiterConfig{RatePerSecond: 1, Burst: 2}, ClientIPKeyer(nil), clk.now)
	h := l.Middleware()(okHandler())
	hit(t, h, "203.0.113.1:1")
	hit(t, h, "203.0.113.1:1") // drained: 0 tokens
	hit(t, h, "203.0.113.2:1") // 1 token left: not idle
	if len(l.limiters) != 2 {
		t.Fatalf("setup: %d buckets", len(l.limiters))
	}
	// 2 s later both are back at burst, but the sweep runs at most once a
	// minute: nothing is deleted before then.
	clk.advance(2 * time.Second)
	hit(t, h, "203.0.113.3:1")
	if len(l.limiters) != 3 {
		t.Fatalf("a sweep ran inside the minute: %d buckets, want 3", len(l.limiters))
	}
	clk.advance(limiterSweepEvery)
	hit(t, h, "203.0.113.4:1")
	// .1 .2 .3 refilled to burst and were swept; only .4 (just used) is held.
	if len(l.limiters) != 1 {
		t.Fatalf("after the sweep %d buckets, want 1", len(l.limiters))
	}
	// A swept key comes back full, not with its old drained state.
	if w := hit(t, h, "203.0.113.1:1"); w.Code != http.StatusNoContent {
		t.Fatalf("a swept key = %d, want a fresh full bucket", w.Code)
	}
}

func TestRateLimiter_ABucketThatIsNotIdleSurvivesTheSweep(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	l := newRateLimiter(LimiterConfig{RatePerSecond: 0.0001, Burst: 1}, ClientIPKeyer(nil), clk.now)
	h := l.Middleware()(okHandler())
	hit(t, h, "203.0.113.1:1") // drained, and it refills in hours
	clk.advance(limiterSweepEvery + time.Second)
	hit(t, h, "203.0.113.2:1")
	if w := hit(t, h, "203.0.113.1:1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a drained bucket was swept: %d", w.Code)
	}
}

func TestRateLimiter_TheKeyCapHoldsAndFailsClosed(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	l := newRateLimiter(LimiterConfig{RatePerSecond: 0.0001, Burst: 1, MaxKeys: 3}, ClientIPKeyer(nil), clk.now)
	h := l.Middleware()(okHandler())
	for _, ip := range []string{"203.0.113.1:1", "203.0.113.2:1", "203.0.113.3:1"} {
		if w := hit(t, h, ip); w.Code != http.StatusNoContent {
			t.Fatalf("%s = %d", ip, w.Code)
		}
	}
	// A fourth NEW key is refused (not admitted unmetered), with a Retry-After.
	w := hit(t, h, "203.0.113.4:1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("new key at the cap = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	if len(l.limiters) != 3 {
		t.Fatalf("%d buckets: the cap did not hold", len(l.limiters))
	}
	// A tracked key is served from its own (drained) bucket, not refused by the cap.
	if w := hit(t, h, "203.0.113.1:1"); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "60" {
		t.Fatalf("tracked key = %d retry-after %q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestRateLimiter_AtTheCapAnImmediateSweepMakesRoomWhenBucketsAreIdle(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	l := newRateLimiter(LimiterConfig{RatePerSecond: 1, Burst: 1, MaxKeys: 2}, ClientIPKeyer(nil), clk.now)
	h := l.Middleware()(okHandler())
	hit(t, h, "203.0.113.1:1")
	hit(t, h, "203.0.113.2:1")
	clk.advance(2 * time.Second) // both refilled, but under a minute since the last sweep
	if w := hit(t, h, "203.0.113.3:1"); w.Code != http.StatusNoContent {
		t.Fatalf("new key with idle buckets at the cap = %d, want 204", w.Code)
	}
}

func TestRetryAfterIsRoundedUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   time.Duration
		want int
	}{{0, 1}, {time.Millisecond, 1}, {999 * time.Millisecond, 1}, {time.Second, 1}, {1001 * time.Millisecond, 2}, {1500 * time.Millisecond, 2}, {59*time.Second + time.Nanosecond, 60}} {
		if got := retryAfterSeconds(tc.in); got != tc.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRateLimiter_RetryAfterHeaderRoundsUp(t *testing.T) {
	t.Parallel()
	clk := newStepClock()
	// One token per 1.5 s: after draining, the wait is 1.5 s, so the header is 2 (truncation said 1).
	l := newRateLimiter(LimiterConfig{RatePerSecond: 1 / 1.5, Burst: 1}, ClientIPKeyer(nil), clk.now)
	h := l.Middleware()(okHandler())
	hit(t, h, "203.0.113.1:1")
	w := hit(t, h, "203.0.113.1:1")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("got %d Retry-After %q, want 429 and 2", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestClientAddress_WarnsOncePerMinuteWhenATrustedProxySendsNoClient(t *testing.T) {
	t.Parallel()
	trusted, _ := ParseTrustedProxies("10.0.0.0/8")
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	clk := newStepClock()
	h := newClientAddress(log, trusted, clk.now)(okHandler())
	send := func(remote, xff string) {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	warns := func() int { return strings.Count(buf.String(), "trusted proxy sent no usable X-Forwarded-For") }

	send("10.0.0.5:1", "198.51.100.7") // good
	send("203.0.113.9:1", "")          // untrusted peer: the header is irrelevant
	if warns() != 0 {
		t.Fatalf("warned for healthy traffic: %s", buf.String())
	}
	send("10.0.0.5:1", "") // missing
	if warns() != 1 {
		t.Fatalf("missing header: %d warns, want 1", warns())
	}
	send("10.0.0.5:1", "garbage") // unusable, same minute: counted but not logged again
	send("10.0.0.5:1", "10.1.1.1")
	if warns() != 1 {
		t.Fatalf("%d warns inside the minute, want 1", warns())
	}
	clk.advance(proxyHeaderWarnEvery)
	send("10.0.0.5:1", "")
	if warns() != 2 {
		t.Fatalf("%d warns after the minute, want 2", warns())
	}
}

func TestClientAddress_StoresTheExactResolvedClientForLaterReaders(t *testing.T) {
	t.Parallel()
	trusted, _ := ParseTrustedProxies("10.0.0.0/8")
	var got string
	h := newClientAddress(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), trusted, time.Now)(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIPFromContext(r.Context()) }))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.5:1"
	r.Header.Set("X-Forwarded-For", "2001:db8:1:2::77")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "2001:db8:1:2::77" {
		t.Fatalf("context client = %q, want the exact address (not the /64)", got)
	}
	if ClientIPFromContext(t.Context()) != "" {
		t.Fatal("a context that never passed ClientAddress must read empty")
	}
}
