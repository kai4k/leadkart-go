package idempotency_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/idempotency"
)

// downstream returns an http.Handler that:
//   - records call count
//   - reads + echoes the request body
//   - sets a Content-Type header
//   - returns the supplied status code
type downstream struct {
	calls  *atomic.Int32
	status int
	body   string
}

func (d *downstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.calls.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(d.status)
		if d.body != "" {
			_, _ = io.WriteString(w, d.body)
		} else {
			// Echo request body so tests can confirm pass-through.
			b, _ := io.ReadAll(r.Body)
			_, _ = w.Write(b)
		}
	})
}

func newPOST(t *testing.T, body, commandID string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/whatever", strings.NewReader(body))
	if commandID != "" {
		req.Header.Set(idempotency.CommandIDHeader, commandID)
	}
	return req
}

func TestMiddleware_NoCommandIDHeader_PassesThrough(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 200, body: `{"ok":true}`}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, nil)(d.handler())

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newPOST(t, `{"x":1}`, ""))

	if calls.Load() != 1 {
		t.Errorf("downstream called %d times, want 1", calls.Load())
	}
	if rec.Code != 200 {
		t.Errorf("status = %d", rec.Code)
	}
	if rec.Header().Get(idempotency.ReplayHeader) != "" {
		t.Error("X-Idempotent-Replay should be absent on no-key path")
	}
}

func TestMiddleware_FirstCall_CachesAndReturns2xx(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 201, body: `{"id":"abc"}`}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, nil)(d.handler())

	cmdID := uuid.NewString()
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newPOST(t, `{"x":1}`, cmdID))

	if calls.Load() != 1 {
		t.Errorf("downstream called %d times, want 1", calls.Load())
	}
	if rec.Code != 201 {
		t.Errorf("status = %d", rec.Code)
	}
	if rec.Body.String() != `{"id":"abc"}` {
		t.Errorf("body = %q", rec.Body.String())
	}
	if rec.Header().Get(idempotency.ReplayHeader) != "" {
		t.Error("X-Idempotent-Replay should be absent on first call")
	}
	if store.Len() != 1 {
		t.Errorf("expected 1 cached record, got %d", store.Len())
	}
}

func TestMiddleware_ReplaySameKeyAndBody_ReturnsCached(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 201, body: `{"id":"abc"}`}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, nil)(d.handler())

	cmdID := uuid.NewString()
	body := `{"x":1}`

	// First call.
	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, newPOST(t, body, cmdID))

	// Replay.
	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, newPOST(t, body, cmdID))

	if calls.Load() != 1 {
		t.Errorf("downstream called %d times, want 1 (replay should NOT call through)", calls.Load())
	}
	if rec2.Code != 201 {
		t.Errorf("replay status = %d", rec2.Code)
	}
	if rec2.Body.String() != `{"id":"abc"}` {
		t.Errorf("replay body = %q", rec2.Body.String())
	}
	if rec2.Header().Get(idempotency.ReplayHeader) != "true" {
		t.Errorf("X-Idempotent-Replay = %q, want \"true\"", rec2.Header().Get(idempotency.ReplayHeader))
	}
	if rec2.Header().Get("Content-Type") == "" {
		t.Error("Content-Type should be replayed")
	}
}

func TestMiddleware_ReplayDifferentBody_Returns422(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 201, body: `{"id":"abc"}`}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, nil)(d.handler())

	cmdID := uuid.NewString()
	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, newPOST(t, `{"x":1}`, cmdID))

	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, newPOST(t, `{"x":2}`, cmdID))

	if calls.Load() != 1 {
		t.Errorf("downstream called %d times, want 1 (mismatch should NOT call through)", calls.Load())
	}
	if rec2.Code != 422 {
		t.Errorf("mismatch status = %d, want 422", rec2.Code)
	}
	if !bytes.Contains(rec2.Body.Bytes(), []byte("idempotency.key_reuse")) {
		t.Errorf("body should contain key_reuse code: %q", rec2.Body.String())
	}
}

func TestMiddleware_MalformedCommandID_Returns400(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 200}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, nil)(d.handler())

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newPOST(t, `{}`, "not-a-uuid"))

	if calls.Load() != 0 {
		t.Errorf("downstream called %d times, want 0 on malformed key", calls.Load())
	}
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("invalid_command_id")) {
		t.Errorf("body should contain invalid_command_id code: %q", rec.Body.String())
	}
}

func TestMiddleware_NonSuccess_NotCached(t *testing.T) {
	// Per Stripe canon: cache only 2xx responses. Transient failures
	// should be retryable without seeing a cached error.
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 500, body: `{"error":"internal"}`}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, nil)(d.handler())

	cmdID := uuid.NewString()
	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, newPOST(t, `{}`, cmdID))
	if rec1.Code != 500 {
		t.Errorf("first call status = %d", rec1.Code)
	}

	// Retry: should call through again, NOT replay the cached failure.
	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, newPOST(t, `{}`, cmdID))
	if calls.Load() != 2 {
		t.Errorf("downstream called %d times, want 2 (failure retry should not be cached)", calls.Load())
	}
	if rec2.Header().Get(idempotency.ReplayHeader) != "" {
		t.Error("X-Idempotent-Replay should be absent on retry of non-cached failure")
	}
}

// TestMiddleware_PerCallerScoping_DoesNotLeakAcrossCallers asserts the
// load-bearing security property the audit-fix migration enforces: two
// callers picking the same X-Command-Id MUST NOT see each other's
// cached response.
func TestMiddleware_PerCallerScoping_DoesNotLeakAcrossCallers(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 201, body: `{"id":"abc"}`}
	store := idempotency.NewInMemoryStore(nil)

	// Inject a deterministic test keyer that flips between two callers
	// based on a header; this isolates the scoping semantics from any
	// auth-stack assumptions.
	keyer := func(r *http.Request) string {
		return "test:" + r.Header.Get("X-Test-Caller")
	}
	mw := idempotency.Middleware(store, nil, 0, keyer)(d.handler())

	cmdID := uuid.NewString()
	body := `{"x":1}`

	// Caller A — first call populates cache.
	rec1 := httptest.NewRecorder()
	req1 := newPOST(t, body, cmdID)
	req1.Header.Set("X-Test-Caller", "A")
	mw.ServeHTTP(rec1, req1)
	if rec1.Code != 201 {
		t.Fatalf("caller A status = %d", rec1.Code)
	}

	// Caller B — same X-Command-Id, same body, different caller scope.
	// MUST execute downstream (no replay), MUST NOT see A's response.
	rec2 := httptest.NewRecorder()
	req2 := newPOST(t, body, cmdID)
	req2.Header.Set("X-Test-Caller", "B")
	mw.ServeHTTP(rec2, req2)

	if calls.Load() != 2 {
		t.Errorf("downstream called %d times, want 2 (per-caller scoping must isolate)", calls.Load())
	}
	if rec2.Header().Get(idempotency.ReplayHeader) != "" {
		t.Error("X-Idempotent-Replay set on cross-caller request — scoping leak")
	}
}

// TestMiddleware_OverSizedBody_Returns413 covers the OWASP API4 cap.
func TestMiddleware_OverSizedBody_Returns413(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 200, body: `ok`}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, nil)(d.handler())

	// 2 MiB > MaxBodyBytes (1 MiB)
	huge := strings.Repeat("a", 2<<20)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newPOST(t, huge, uuid.NewString()))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if calls.Load() != 0 {
		t.Errorf("downstream called %d times, want 0", calls.Load())
	}
}

func TestMiddleware_TTLExpiry_RetriesDownstream(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clock := now
	store := idempotency.NewInMemoryStore(func() time.Time { return clock })
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 201, body: `{"id":"abc"}`}
	mw := idempotency.Middleware(store, func() time.Time { return clock }, time.Hour, nil)(d.handler())

	cmdID := uuid.NewString()
	body := `{}`

	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, newPOST(t, body, cmdID))

	// Advance past TTL.
	clock = now.Add(2 * time.Hour)

	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, newPOST(t, body, cmdID))
	if calls.Load() != 2 {
		t.Errorf("downstream called %d times, want 2 (TTL expired should re-execute)", calls.Load())
	}
}
