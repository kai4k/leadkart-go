package httpmw

import "net/http"

// DefaultMaxBodyBytes caps every request body the public chain accepts
// (OWASP API4 — Unrestricted Resource Consumption). 1 MiB matches the
// idempotency middleware's hash cap; no LeadKart JSON payload approaches it.
const DefaultMaxBodyBytes = 1 << 20 // 1 MiB

// BodyLimit wraps every request body in [http.MaxBytesReader] so a handler's
// json.Decoder (or any reader) can never buffer an unbounded body into memory.
// Reads past the cap fail with MaxBytesError — handlers surface their normal
// malformed-body 4xx, and MaxBytesReader independently arranges the 413 /
// connection close semantics.
//
// Previously only the X-Command-Id idempotency path capped bodies; an
// authenticated caller omitting that header could stream an arbitrarily large
// JSON body. This middleware closes that gap chain-wide.
func BodyLimit(maxBytes int64) Middleware {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
