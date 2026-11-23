package httpmw

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// errCodeInvalidQuery is the wire-stable code for a query string the server
// cannot read (the modules use the same code for their own query refusals).
const errCodeInvalidQuery = "invalid_query"

// queryFieldInvalidCharacters is the code under errors.<parameter>.
const queryFieldInvalidCharacters = "invalid_characters"

// unreadableNameKey is the errors key for a parameter whose NAME is unreadable
// (the name itself cannot be echoed).
const unreadableNameKey = "query"

// RefuseUnreadableQuery answers 400 invalid_query when a query-string name or
// value holds U+0000 or bytes that are not UTF-8, for every route.
//
// A query value is text the handlers pass to PostgreSQL (a search term, a
// filter), and PostgreSQL can take neither: the statement fails with SQLSTATE
// 22021 and the request used to answer 500 from whichever read met the value
// first. Nobody can type such a value, so refusing it here, once, loses nothing
// and no handler has to remember to. The body names each offending parameter
// under errors, with the code invalid_characters.
//
// A pair the URL parser cannot decode at all (a malformed %-escape) is not this
// rule's business: the parser drops it and handlers read the value as absent,
// as they always have.
func RefuseUnreadableQuery() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				if bad := unreadableQueryParams(r); len(bad) > 0 {
					writeUnreadableQuery(w, bad)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// unreadableQueryParams returns the errors map for r's query: one entry per
// parameter with an unreadable value, and [unreadableNameKey] for unreadable names.
func unreadableQueryParams(r *http.Request) map[string][]string {
	var bad map[string][]string
	mark := func(name string) {
		if bad == nil {
			bad = map[string][]string{}
		}
		bad[name] = []string{queryFieldInvalidCharacters}
	}
	for name, values := range r.URL.Query() {
		if unreadableText(name) {
			mark(unreadableNameKey)
			continue
		}
		for _, v := range values {
			if unreadableText(v) {
				mark(name)
				break
			}
		}
	}
	return bad
}

// unreadableText reports whether PostgreSQL would refuse s as text.
func unreadableText(s string) bool {
	return strings.IndexByte(s, 0) >= 0 || !utf8.ValidString(s)
}

func writeUnreadableQuery(w http.ResponseWriter, fields map[string][]string) {
	const msg = "the query string holds characters the server cannot read"
	body, err := json.Marshal(struct {
		Type    string              `json:"type"`
		Title   string              `json:"title"`
		Status  int                 `json:"status"`
		Detail  string              `json:"detail"`
		Error   string              `json:"error"`
		Message string              `json:"message"`
		Errors  map[string][]string `json:"errors"`
	}{
		Type: "https://leadkart.api/errors/" + errCodeInvalidQuery, Title: http.StatusText(http.StatusBadRequest),
		Status: http.StatusBadRequest, Detail: msg, Error: errCodeInvalidQuery, Message: msg, Errors: fields,
	})
	if err != nil { // a map of strings always encodes; answer the status even so
		body = []byte(`{"error":"` + errCodeInvalidQuery + `"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(body)
}
