package httpmw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/httpmw"
)

func TestCorrelation_AbsentHeader_MintsAndEchoes(t *testing.T) {
	t.Parallel()
	var observedID string
	mw := httpmw.Correlation()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observedID = httpmw.CorrelationIDFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if observedID == "" {
		t.Fatal("ctx-bound correlation ID empty — middleware did not mint")
	}
	if _, err := uuid.Parse(observedID); err != nil {
		t.Fatalf("minted ID is not a UUID: %q (%v)", observedID, err)
	}
	if got := rec.Header().Get(httpmw.CorrelationIDHeader); got != observedID {
		t.Fatalf("response header: got %q want %q", got, observedID)
	}
}

func TestCorrelation_PresentHeader_PassedThrough(t *testing.T) {
	t.Parallel()
	wantID := uuid.NewString()
	var observedID string
	mw := httpmw.Correlation()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observedID = httpmw.CorrelationIDFromContext(r.Context())
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set(httpmw.CorrelationIDHeader, wantID)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if observedID != wantID {
		t.Fatalf("ctx ID: got %q want %q (incoming header dropped)", observedID, wantID)
	}
	if got := rec.Header().Get(httpmw.CorrelationIDHeader); got != wantID {
		t.Fatalf("echoed header: got %q want %q", got, wantID)
	}
}

func TestCorrelation_MalformedHeader_Replaced(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hdr  string
	}{
		{"not a UUID", "abc-not-a-uuid"},
		{"giant string", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"whitespace only", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var observedID string
			mw := httpmw.Correlation()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				observedID = httpmw.CorrelationIDFromContext(r.Context())
			}))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.Header.Set(httpmw.CorrelationIDHeader, tc.hdr)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if observedID == tc.hdr {
				t.Fatalf("malformed inbound header was not replaced: %q", observedID)
			}
			if _, err := uuid.Parse(observedID); err != nil {
				t.Fatalf("replacement is not a UUID: %q", observedID)
			}
		})
	}
}

func TestCorrelationIDFromContext_NoBinding_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := httpmw.CorrelationIDFromContext(t.Context()); got != "" {
		t.Fatalf("ctx without binding: got %q want empty", got)
	}
}
