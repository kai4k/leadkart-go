package httpmw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

func TestSecurityHeaders_SetsAllFourOnEveryResponse(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := httpmw.SecurityHeaders()(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/anything", nil)
	h.ServeHTTP(rec, req)

	want := map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"Referrer-Policy":           "no-referrer",
	}
	for k, v := range want {
		got := rec.Header().Get(k)
		if got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

func TestSecurityHeaders_SetsHeadersOnErrorResponses(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	h := httpmw.SecurityHeaders()(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/anything", nil)
	h.ServeHTTP(rec, req)

	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options not set on 500 response")
	}
}
