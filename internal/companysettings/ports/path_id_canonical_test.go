package ports

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Path ids go through the one canonical-id function: any valid spelling parses
// to the same UUID; a padded, wrapped or nil id is a 400 (uuid.Parse alone
// accepts " <uuid> " because it strips the first and last byte of 38 characters).
func TestParsePathUUID_Canonical(t *testing.T) {
	t.Parallel()
	const canon = "0190a000-0000-7000-8000-0000000000ab"
	for _, in := range []string{canon, "0190A000-0000-7000-8000-0000000000AB", "urn:uuid:" + canon, "{" + canon + "}"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		r.SetPathValue("rateId", in)
		got, ok := parsePathUUID(httptest.NewRecorder(), r, "rateId")
		if !ok || got.String() != canon {
			t.Errorf("parsePathUUID(%q) = %v, %v; want %q, true", in, got, ok, canon)
		}
	}
	for _, in := range []string{" " + canon + " ", " " + canon, canon + "\n", "x" + canon + "y", "00000000-0000-0000-0000-000000000000", ""} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		r.SetPathValue("rateId", in)
		rec := httptest.NewRecorder()
		if _, ok := parsePathUUID(rec, r, "rateId"); ok || rec.Code != http.StatusBadRequest {
			t.Errorf("parsePathUUID(%q): ok=%v status=%d, want false 400", in, ok, rec.Code)
		}
	}
}
