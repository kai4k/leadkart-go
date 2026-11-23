package idempotency_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
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

// quietLog discards everything: most tests do not assert on the log.
func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// defaultTestKeyer is the scope the middleware used to default to: the tenant
// from the context, else the socket address. Production has no default (the
// composition root passes authn.IdempotencyCallerKeyer); these tests exercise
// the middleware's own behaviour with a minimal scope.
func defaultTestKeyer(r *http.Request) string {
	if id, ok := tenancy.FromContext(r.Context()); ok && !id.IsZero() {
		return "tenant:" + id.String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return ""
	}
	return "anon:" + host
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
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, nil, 0, keyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())

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
	mw := idempotency.Middleware(store, func() time.Time { return clock }, time.Hour, defaultTestKeyer, quietLog())(d.handler())

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

// Safe methods carry no command: the header is ignored (not even validated), the
// answer is never stored, and a second identical GET runs again.
func TestMiddleware_SafeMethodsIgnoreTheHeaderAndStoreNothing(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			calls := &atomic.Int32{}
			d := &downstream{calls: calls, status: 200, body: `{"ok":true}`}
			store := idempotency.NewInMemoryStore(nil)
			mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(d.handler())
			key := uuid.NewString()
			for i := range 2 {
				req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/things", nil)
				req.Header.Set(idempotency.CommandIDHeader, key)
				rec := httptest.NewRecorder()
				mw.ServeHTTP(rec, req)
				if rec.Header().Get(idempotency.ReplayHeader) != "" {
					t.Fatalf("%s call %d was replayed", method, i+1)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("handler ran %d times, want 2 (nothing replayed)", calls.Load())
			}
			// A malformed id on a safe method is not a 400: the header is ignored.
			req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/things", nil)
			req.Header.Set(idempotency.CommandIDHeader, "not-a-uuid")
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("malformed id on %s = %d, want it ignored", method, rec.Code)
			}
		})
	}
}

// A keyer that says Ineligible (credential routes, anonymous callers) makes the
// middleware ignore the header entirely: no 400 for a malformed one, nothing
// stored, the body never buffered, the handler runs every time.
func TestMiddleware_IneligibleRequestsPassThroughAndStoreNothing(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	d := &downstream{calls: calls, status: 200, body: `{"refresh_token":"secret"}`}
	store := idempotency.NewInMemoryStore(nil)
	mw := idempotency.Middleware(store, nil, 0, func(*http.Request) string { return idempotency.Ineligible }, quietLog())(d.handler())
	key := uuid.NewString()
	for range 2 {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, newPOST(t, `{"password":"hunter2"}`, key))
		if rec.Code != 200 || rec.Header().Get(idempotency.ReplayHeader) != "" {
			t.Fatalf("ineligible request: %d replay=%q", rec.Code, rec.Header().Get(idempotency.ReplayHeader))
		}
	}
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newPOST(t, `{}`, "not-a-uuid"))
	if rec.Code != 200 {
		t.Fatalf("malformed id on an ineligible request = %d, want it ignored", rec.Code)
	}
	if calls.Load() != 3 {
		t.Fatalf("handler ran %d times, want 3", calls.Load())
	}
	if n := store.Len(); n != 0 {
		t.Fatalf("%d records stored for ineligible requests", n)
	}
}

func TestMiddleware_RefusesToBuildWithoutAKeyer(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic: there is no default scope")
		}
	}()
	idempotency.Middleware(idempotency.NewInMemoryStore(nil), nil, 0, nil, quietLog())
}

// notNullBodyStore behaves like the Postgres store, whose response_body column is
// NOT NULL: a record with a nil body is refused.
type notNullBodyStore struct{ *idempotency.InMemoryStore }

func (s notNullBodyStore) Put(ctx context.Context, r idempotency.Record) error {
	if r.ResponseBody == nil {
		return errors.New(`null value in column "response_body" violates not-null constraint`)
	}
	return s.InMemoryStore.Put(ctx, r)
}

// A 204 answer has no body. It must still be stored (with an empty, non-nil
// body) so a retry of the same command replays the 204 instead of running the
// command a second time: before this, NO 204 command was ever stored.
func TestMiddleware_NoContentAnswerIsStoredAndReplayed(t *testing.T) {
	t.Parallel()
	calls := &atomic.Int32{}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	store := notNullBodyStore{idempotency.NewInMemoryStore(nil)}
	mw := idempotency.Middleware(store, nil, 0, defaultTestKeyer, quietLog())(next)

	cmdID := uuid.NewString()
	first := httptest.NewRecorder()
	mw.ServeHTTP(first, newPOST(t, `{"x":1}`, cmdID))
	if first.Code != http.StatusNoContent || first.Header().Get(idempotency.ReplayHeader) != "" {
		t.Fatalf("first call: status %d replay=%q, want a fresh 204", first.Code, first.Header().Get(idempotency.ReplayHeader))
	}
	if store.Len() != 1 {
		t.Fatalf("a 204 answer was not stored: %d records", store.Len())
	}

	again := httptest.NewRecorder()
	mw.ServeHTTP(again, newPOST(t, `{"x":1}`, cmdID))
	if calls.Load() != 1 {
		t.Errorf("the command ran %d times, want 1 (the retry must replay)", calls.Load())
	}
	if again.Code != http.StatusNoContent || again.Header().Get(idempotency.ReplayHeader) != "true" || again.Body.Len() != 0 {
		t.Errorf("retry: status %d replay=%q body %q, want 204 with X-Idempotent-Replay: true and no body",
			again.Code, again.Header().Get(idempotency.ReplayHeader), again.Body.String())
	}
}

// failingStore refuses every write.
type failingStore struct{ *idempotency.InMemoryStore }

func (failingStore) Put(context.Context, idempotency.Record) error {
	return errors.New("store is down")
}

// A failed store write must not change the answer the caller gets, and must not
// be silent: it means the command cannot be replayed, so it is logged.
func TestMiddleware_AFailedStoreWriteIsLoggedNotSwallowed(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := &downstream{calls: &atomic.Int32{}, status: 201, body: `{"id":"abc"}`}
	mw := idempotency.Middleware(failingStore{idempotency.NewInMemoryStore(nil)}, nil, 0, defaultTestKeyer, log)(d.handler())

	cmdID := uuid.NewString()
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newPOST(t, `{"x":1}`, cmdID))
	if rec.Code != 201 || rec.Body.String() != `{"id":"abc"}` {
		t.Fatalf("the answer changed because the store failed: %d %q", rec.Code, rec.Body.String())
	}
	out := logged.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "store is down") || !strings.Contains(out, cmdID) {
		t.Fatalf("a failed store write must be logged at ERROR with its cause and the command id, got: %q", out)
	}
}

func TestMiddleware_RefusesToBuildWithoutALogger(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Middleware built without a logger: a failed store write would be silent")
		}
	}()
	idempotency.Middleware(idempotency.NewInMemoryStore(nil), nil, 0, defaultTestKeyer, nil)
}
