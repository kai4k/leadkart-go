package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CommandIDHeader is the wire-stable header name. Mirrors Stripe's
// Idempotency-Key but uses the LeadKart-canonical X-Command-Id form
// per `messaging.md`.
const CommandIDHeader = "X-Command-Id"

// ReplayHeader is set on responses returned from a cached record so
// the client can distinguish "fresh execution" from "cached replay".
const ReplayHeader = "X-Idempotent-Replay"

// DefaultTTL is the default idempotency window. 24 hours per Stripe
// canon — long enough to survive client retries across a partial
// outage, short enough that the storage doesn't bloat.
const DefaultTTL = 24 * time.Hour

// MaxBodyBytes caps the per-request body size the middleware will
// buffer for hashing. Per OWASP API4 (Unrestricted Resource
// Consumption): an unbounded io.ReadAll on the request body is a
// trivial soft-DoS vector — a 10 GiB upload is fully buffered into
// memory before any application logic runs. 1 MiB is the canonical
// Stripe / GitHub API ceiling for JSON-shaped POST/PUT/DELETE
// bodies. Routes that need larger bodies (file uploads) MUST opt
// out of the idempotency middleware OR negotiate a larger ceiling
// in a follow-up.
const MaxBodyBytes = 1 << 20 // 1 MiB

// Wire-stable error codes for the body shape.
const (
	errCodeInvalidCommandID = "idempotency.invalid_command_id"
	errCodeKeyReuse         = "idempotency.key_reuse"
)

// Middleware wraps next in idempotency handling per [CommandIDHeader].
//
// Behaviour:
//
//   - Header absent → call next directly (opt-in).
//   - Header malformed (not a UUID) → 400 idempotency.invalid_command_id.
//   - Cached match → replay stored response with X-Idempotent-Replay: true.
//   - Cached mismatch (same key, different body hash) → 422
//     idempotency.key_reuse.
//   - First execution + 2xx response → cache verbatim.
//   - First execution + non-2xx → DO NOT cache (failures are transient
//     by default; client should be free to retry without seeing a
//     cached failure response).
//
// Per Stripe canon (https://stripe.com/blog/idempotency 2018):
// caching only successful responses lets transient failures (network,
// upstream timeout) be retried legitimately.
//
// `now` is the clock function (testable via clock.Set in tests).
// `ttl` is the per-record retention; 0 means [DefaultTTL].
func Middleware(store Store, now func() time.Time, ttl time.Duration) func(http.Handler) http.Handler {
	if store == nil {
		panic("idempotency: Middleware store required")
	}
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := strings.TrimSpace(r.Header.Get(CommandIDHeader))
			if raw == "" {
				// Opt-in: no header, no idempotency. Call through.
				next.ServeHTTP(w, r)
				return
			}
			key, err := uuid.Parse(raw)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, errCodeInvalidCommandID,
					"X-Command-Id must be a UUID")
				return
			}

			// Read + hash the request body. Bounded by MaxBodyBytes
			// (OWASP API4 — http.MaxBytesReader returns a 413
			// Request Entity Too Large via its sentinel error so
			// the response shape stays correct). We always rebuild
			// r.Body afterwards so downstream handlers can decode it.
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				// MaxBytesReader's sentinel produces a 413 on the
				// underlying ResponseWriter as a side-effect of Read
				// — but only if WriteHeader hasn't been called. We
				// emit our own structured 413 either way so the
				// shape matches the rest of the API.
				writeJSONError(w, http.StatusRequestEntityTooLarge, errCodeInvalidCommandID,
					"request body too large or unreadable")
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			bodyHash := sha256.Sum256(body)

			// Check cache.
			rec, err := store.Get(r.Context(), key, bodyHash)
			switch {
			case errors.Is(err, ErrBodyMismatch):
				writeJSONError(w, http.StatusUnprocessableEntity, errCodeKeyReuse,
					"X-Command-Id was previously used with a different request body")
				return
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError, errCodeInvalidCommandID,
					"idempotency lookup failed")
				return
			}
			if rec.Key != uuid.Nil {
				// Cached match — replay verbatim.
				for k, v := range rec.ResponseHeaders {
					w.Header().Set(k, v)
				}
				w.Header().Set(ReplayHeader, "true")
				w.WriteHeader(rec.ResponseStatus)
				_, _ = w.Write(rec.ResponseBody)
				return
			}

			// First execution — capture the downstream response.
			rec2 := &recorder{
				ResponseWriter: w,
				header:         w.Header(),
				body:           &bytes.Buffer{},
				status:         http.StatusOK,
			}
			next.ServeHTTP(rec2, r)

			// Cache only 2xx responses — transient failures should
			// be retryable.
			if rec2.status < 200 || rec2.status >= 300 {
				return
			}
			contentType := rec2.header.Get("Content-Type")
			recHeaders := map[string]string{}
			if contentType != "" {
				recHeaders["Content-Type"] = contentType
			}
			n := now()
			_ = store.Put(r.Context(), Record{
				Key:             key,
				BodyHash:        bodyHash,
				ResponseStatus:  rec2.status,
				ResponseBody:    rec2.body.Bytes(),
				ResponseHeaders: recHeaders,
				CreatedAt:       n,
				ExpiresAt:       n.Add(ttl),
			})
		})
	}
}

// recorder wraps http.ResponseWriter to capture status + body for
// later cache write. Headers are written through to the underlying
// ResponseWriter at the same moment they're surfaced to the client —
// the recorder just snapshots what was set.
type recorder struct {
	http.ResponseWriter
	header http.Header
	body   *bytes.Buffer
	status int
	wrote  bool
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// writeJSONError emits a stable error shape for the middleware's own
// 400/422/500 responses. Mirrors the application-layer ErrorResponse
// shape in `internal/identity/ports/errcodes.go`; we redefine here to
// keep this package free of cross-cutting deps.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error   string `json:"error"`
		Message string `json:"message,omitempty"`
	}{
		Error:   code,
		Message: message,
	})
}
