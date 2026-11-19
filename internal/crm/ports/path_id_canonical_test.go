package ports

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Path ids are canonicalised before they reach a command: any spelling
// uuid.Parse accepts comes out lowercase-hyphenated.
func TestPathIDParsers_Canonical(t *testing.T) {
	t.Parallel()
	const canon = "0190a000-0000-7000-8000-0000000000ab"
	tests := []struct {
		name  string
		parse func(w http.ResponseWriter, r *http.Request) (string, bool)
		param string
	}{
		{"leadId", func(w http.ResponseWriter, r *http.Request) (string, bool) {
			id, ok := parseLeadID(w, r)
			return string(id), ok
		}, "leadId"},
		{"reminderId", func(w http.ResponseWriter, r *http.Request) (string, bool) {
			id, ok := parseReminderID(w, r)
			return string(id), ok
		}, "reminderId"},
	}
	for _, tc := range tests {
		for _, in := range []string{canon, strings.ToUpper(canon), "urn:uuid:" + canon, "{" + canon + "}", strings.ReplaceAll(canon, "-", "")} {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			r.SetPathValue(tc.param, in)
			got, ok := tc.parse(httptest.NewRecorder(), r)
			if !ok || got != canon {
				t.Errorf("%s(%q) = %q, %v; want %q, true", tc.name, in, got, ok, canon)
			}
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		r.SetPathValue(tc.param, "nope")
		rec := httptest.NewRecorder()
		if _, ok := tc.parse(rec, r); ok || rec.Code != http.StatusBadRequest {
			t.Errorf("%s(nope): ok=%v status=%d, want false 400", tc.name, ok, rec.Code)
		}
	}
}
