package httpmw_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

// safeBuffer wraps bytes.Buffer for race-safe slog writes.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRecover_HandlerPanic_Returns500AndLogs(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelError}))

	mw := httpmw.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"internal_error"`) {
		t.Fatalf("body missing internal_error code: %q", body)
	}
	logs := buf.String()
	if !strings.Contains(logs, `"panic":"kaboom"`) {
		t.Fatalf("log missing panic value: %q", logs)
	}
	if !strings.Contains(logs, `"stack"`) {
		t.Fatalf("log missing stack trace: %q", logs)
	}
}

func TestRecover_HandlerPanic_LogsCorrelationID(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelError}))

	chain := httpmw.Correlation()(httpmw.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set(httpmw.CorrelationIDHeader, "00000000-0000-7000-8000-000000000099")
	chain.ServeHTTP(rec, req)

	logs := buf.String()
	if !strings.Contains(logs, `"correlation_id":"00000000-0000-7000-8000-000000000099"`) {
		t.Fatalf("log missing correlation_id: %q", logs)
	}
}

func TestRecover_NormalRequest_PassesThrough(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewJSONHandler(&safeBuffer{}, nil))
	called := false
	mw := httpmw.Recover(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if !called {
		t.Fatal("inner handler not invoked")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
}

func TestRecover_AbortHandler_PropagatesToStdlib(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewJSONHandler(&safeBuffer{}, nil))
	mw := httpmw.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	rec := httptest.NewRecorder()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("ErrAbortHandler was swallowed; expected re-panic for stdlib server")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("re-panic value: got %v want ErrAbortHandler", r)
		}
	}()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
}
