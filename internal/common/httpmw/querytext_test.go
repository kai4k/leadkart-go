package httpmw_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

type queryRefusal struct {
	Status int                 `json:"status"`
	Error  string              `json:"error"`
	Type   string              `json:"type"`
	Errors map[string][]string `json:"errors"`
}

func serveQuery(t *testing.T, rawQuery string) (rec *httptest.ResponseRecorder, reached bool) {
	t.Helper()
	h := httpmw.RefuseUnreadableQuery()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/anything", nil)
	req.URL.RawQuery = rawQuery
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, reached
}

// A query string is text the handlers hand to PostgreSQL (a search, a filter).
// PostgreSQL cannot take U+0000 or bytes that are not UTF-8, so such a value
// used to surface as a 500 from whichever statement met it first. It is a
// refusal, for every route and every parameter, decided before any handler.
func TestRefuseUnreadableQuery_BadBytesAreA400NamingTheParameter(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		query string
		want  []string // parameters named under errors
	}{
		"NUL at the start":              {"q=%00ab", []string{"q"}},
		"NUL at the end":                {"q=ab%00", []string{"q"}},
		"bytes that are not UTF-8":      {"search=%ff%fe", []string{"search"}},
		"a truncated UTF-8 sequence":    {"q=ex%c3", []string{"q"}},
		"an overlong encoding":          {"city=%c0%af", []string{"city"}},
		"a lone surrogate":              {"q=%ed%a0%80", []string{"q"}},
		"the second of two values":      {"a=ok&a=%00", []string{"a"}},
		"one bad among good parameters": {"limit=5&q=%ff&page_size=10", []string{"q"}},
		"two bad parameters":            {"manufacturer=%00&dosage_form=%fe", []string{"dosage_form", "manufacturer"}},
		"a bad parameter name":          {"%00=1", []string{"query"}},
		"a bad name that is not UTF-8":  {"%ff%fe=1&q=fine", []string{"query"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec, reached := serveQuery(t, tc.query)
			if reached {
				t.Fatal("the handler ran: the query must be refused before it")
			}
			var got queryRefusal
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
			}
			if rec.Code != http.StatusBadRequest || got.Status != http.StatusBadRequest || got.Error != "invalid_query" || got.Type != "https://leadkart.api/errors/invalid_query" {
				t.Fatalf("answer = %d %s, want 400 invalid_query with its problem type", rec.Code, rec.Body.String())
			}
			named := make([]string, 0, len(got.Errors))
			for k, codes := range got.Errors {
				named = append(named, k)
				if !slices.Equal(codes, []string{"invalid_characters"}) {
					t.Errorf("errors[%q] = %v, want [invalid_characters]", k, codes)
				}
			}
			slices.Sort(named)
			if !slices.Equal(named, tc.want) {
				t.Errorf("errors names %v, want %v", named, tc.want)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type %q", ct)
			}
		})
	}
}

// Everything a person can legitimately type passes untouched.
func TestRefuseUnreadableQuery_ReadableQueriesPass(t *testing.T) {
	t.Parallel()
	for name, query := range map[string]string{
		"no query":                   "",
		"plain":                      "q=ram&limit=5",
		"Hindi":                      "q=%E0%A4%B0%E0%A4%BE%E0%A4%AE",
		"an emoji":                   "q=%F0%9F%92%8A",
		"spaces, plus and newline":   "q=a+b%20c%0Ad",
		"an empty value":             "q=&status=",
		"percent signs and wildcard": "q=50%25+_off%5C",
		"a base64 cursor":            "cursor=eyJzIjoiMjAyNi0wNS0wN1QxMjowMDowMFoiLCJpIjoiMDE5MGIwMDAifQ",
		// A pair the URL parser cannot decode is dropped by it, as before: every
		// handler already reads the value as absent. It is not this rule's business.
		"a malformed escape": "q=%zz&limit=5",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if rec, reached := serveQuery(t, query); !reached || rec.Code != http.StatusNoContent {
				t.Fatalf("query %q: reached=%v status=%d, want the handler's 204", query, reached, rec.Code)
			}
		})
	}
}
