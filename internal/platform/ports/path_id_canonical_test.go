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
		{"id", func(w http.ResponseWriter, r *http.Request) (string, bool) {
			id, ok := parsePathUUID(w, r, "id", "invalid_id")
			return string(id), ok
		}, "id"},
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

// The canonical form rejects, never trims: a padded id or the nil UUID is a 400.
func TestParsePathUUID_RejectsWhitespaceAndNil(t *testing.T) {
	t.Parallel()
	const canon = "0190a000-0000-7000-8000-0000000000ab"
	for _, in := range []string{" " + canon, canon + " ", canon + "\n", " " + canon, "", "00000000-0000-0000-0000-000000000000"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		r.SetPathValue("id", in)
		rec := httptest.NewRecorder()
		if got, ok := parsePathUUID(rec, r, "id", "invalid_id"); ok || rec.Code != http.StatusBadRequest {
			t.Errorf("parsePathUUID(%q) = %q, %v status %d; want false 400", in, got, ok, rec.Code)
		}
	}
}
