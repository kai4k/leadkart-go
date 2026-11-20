package httpmw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

// fakeClock lets a test say how long the handler "took" and records what the
// middleware then waits, so the floor is asserted without sleeping.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waits   []time.Duration
	waitCtx context.Context
}

func (c *fakeClock) pacer() httpmw.Pacer {
	return httpmw.Pacer{
		Now: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now },
		Wait: func(ctx context.Context, d time.Duration) {
			c.mu.Lock()
			c.waits = append(c.waits, d)
			c.waitCtx = ctx
			c.now = c.now.Add(d)
			c.mu.Unlock()
		},
	}
}

func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestMinDuration_WaitsOutTheRemainderOfTheFloor(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	h := httpmw.MinDurationWithPacer(300*time.Millisecond, clk.pacer())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		clk.advance(40 * time.Millisecond) // the known-account path did real work
		w.Header().Set("X-Probe", "1")
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
	if len(clk.waits) != 1 || clk.waits[0] != 260*time.Millisecond {
		t.Fatalf("waits = %v, want exactly [260ms] (floor minus handler time)", clk.waits)
	}
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Probe") != "1" {
		t.Fatalf("response not preserved: %d %v", rec.Code, rec.Header())
	}
}

// The fast branch (no account) and the slow one (account) leave the middleware
// at the same instant: both end at start+floor.
func TestMinDuration_FastAndSlowHandlersFinishAtTheSameInstant(t *testing.T) {
	t.Parallel()
	for _, handlerTime := range []time.Duration{0, 5 * time.Millisecond, 120 * time.Millisecond} {
		start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
		clk := &fakeClock{now: start}
		h := httpmw.MinDurationWithPacer(300*time.Millisecond, clk.pacer())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			clk.advance(handlerTime)
			w.WriteHeader(http.StatusNoContent)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
		if got := clk.now.Sub(start); got != 300*time.Millisecond {
			t.Errorf("handler took %v: response released after %v, want exactly the 300ms floor", handlerTime, got)
		}
	}
}

func TestMinDuration_SlowHandlerIsNotDelayedFurther(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	h := httpmw.MinDurationWithPacer(300*time.Millisecond, clk.pacer())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		clk.advance(700 * time.Millisecond)
		_, _ = w.Write([]byte("late")) // implicit 200
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
	if len(clk.waits) != 0 {
		t.Fatalf("waited %v after a handler already over the floor", clk.waits)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "late" {
		t.Fatalf("response not preserved: %d %q", rec.Code, rec.Body.String())
	}
}

func TestMinDuration_WaitReceivesTheRequestContext(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	h := httpmw.MinDurationWithPacer(time.Second, clk.pacer())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "POST", "/", nil))
	if clk.waitCtx != ctx {
		t.Fatal("the wait is not bound to the request context, so a client disconnect could not end it")
	}
}

// The real pacer, under virtual time: an already-cancelled request must end the
// wait at once. If the wait ran its course, virtual time would advance by the
// whole floor; it must not move at all.
func TestMinDuration_ClientCancelEndsTheRealWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		h := httpmw.MinDuration(time.Hour)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		start := time.Now() // arch-test:wall-clock — virtual time inside synctest.Test
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "POST", "/", nil))
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("a cancelled request waited %v of virtual time", elapsed)
		}
	})
}

// And without a cancel the same real pacer holds the response for the whole floor.
func TestMinDuration_RealWaitHoldsTheResponseForTheFloor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := httpmw.MinDuration(300 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		start := time.Now() // arch-test:wall-clock — virtual time inside synctest.Test
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/", nil))
		if elapsed := time.Since(start); elapsed != 300*time.Millisecond {
			t.Fatalf("released after %v of virtual time, want exactly 300ms", elapsed)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

func TestMinDuration_RejectsANonPositiveFloor(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic for a zero floor")
		}
	}()
	httpmw.MinDuration(0)
}
