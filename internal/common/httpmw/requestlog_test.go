package httpmw_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

func TestRequestLog_2xxLogsInfo(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mw := httpmw.RequestLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil))

	logs := buf.String()
	if !strings.Contains(logs, `"level":"INFO"`) {
		t.Fatalf("2xx not logged at INFO: %s", logs)
	}
	if !strings.Contains(logs, `"status":200`) {
		t.Fatalf("status field missing: %s", logs)
	}
	if !strings.Contains(logs, `"path":"/api/v1/health"`) {
		t.Fatalf("path field missing: %s", logs)
	}
}

func TestRequestLog_4xxLogsWarn(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mw := httpmw.RequestLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	logs := buf.String()
	if !strings.Contains(logs, `"level":"WARN"`) {
		t.Fatalf("4xx not logged at WARN: %s", logs)
	}
}

func TestRequestLog_5xxLogsError(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mw := httpmw.RequestLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	logs := buf.String()
	if !strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("5xx not logged at ERROR: %s", logs)
	}
}

func TestRequestLog_BindsCorrelationAndTenant(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Inner handler asserts ctx already has correlation + tenant when
	// the request log fires (it'll fire after handler returns).
	chain := httpmw.Correlation()(httpmw.RequestLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bind tenant inside handler — RequireFreshStamp does this in
		// production; in this test we simulate by writing to the
		// request scope inline.
		ctx := tenancy.WithID(r.Context(), tenancy.ID("019dfe62-d263-7a20-b7de-08df2621c8eb"))
		*r = *r.WithContext(ctx)
		w.WriteHeader(http.StatusOK)
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set(httpmw.CorrelationIDHeader, "00000000-0000-7000-8000-000000000042")
	chain.ServeHTTP(rec, req)

	logs := buf.String()
	if !strings.Contains(logs, `"correlation_id":"00000000-0000-7000-8000-000000000042"`) {
		t.Fatalf("correlation_id missing from log: %s", logs)
	}
	if !strings.Contains(logs, `"tenant_id":"019dfe62-d263-7a20-b7de-08df2621c8eb"`) {
		t.Fatalf("tenant_id missing from log (bound after entry, captured at exit): %s", logs)
	}
}

func TestRequestLog_DefaultsImplicitWriteTo200(t *testing.T) {
	t.Parallel()
	// A handler that writes a body without calling WriteHeader returns
	// 200 per stdlib. The capture must record 200, not 0.
	buf := &safeBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mw := httpmw.RequestLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`hello`))
	}))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	logs := buf.String()
	if !strings.Contains(logs, `"status":200`) {
		t.Fatalf("implicit 200 not captured: %s", logs)
	}
	if !strings.Contains(logs, `"bytes":5`) {
		t.Fatalf("byte count not recorded: %s", logs)
	}
}
