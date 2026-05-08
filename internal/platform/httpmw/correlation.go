package httpmw

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// CorrelationIDHeader is the wire-stable header name. Mirrors the
// W3C-trace `traceparent` ergonomics but uses our own name so it
// stays stable even if we swap tracing backends. The value is a
// UUIDv7 — sortable, monotonic, suitable for cache keys + log
// indexing.
const CorrelationIDHeader = "X-Correlation-ID"

// correlationCtxKey is the unexported tag for [WithCorrelationID].
type correlationCtxKey struct{}

// WithCorrelationID binds the correlation ID onto ctx. Used by the
// [Correlation] middleware and by tests that need to inject a
// synthetic value.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, id)
}

// CorrelationIDFromContext returns the correlation ID bound to ctx,
// or empty string if none was set. Returns empty rather than ok-bool
// because the only consumer (logger fields) wants a string anyway.
func CorrelationIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(correlationCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// Correlation reads the [CorrelationIDHeader] from the request; if
// absent or malformed, mints a fresh UUIDv7. The resolved ID is
// stashed on ctx + echoed in the response header so client SDKs
// (browser Network tab, curl -i) can pin a single ID across the
// request lifecycle for support escalations.
//
// Order policy: this is the OUTERMOST app-level middleware (after
// otelhttp). Putting it before the request logger means every log
// line on the request — including panic logs from the recover layer
// — carries the same ID. Putting it after the logger leaves the
// "request started" log line correlated to a different ID than the
// rest of the request's logs.
func Correlation() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSpace(r.Header.Get(CorrelationIDHeader))
			if !validCorrelationID(id) {
				id = uuid.NewString()
			}
			w.Header().Set(CorrelationIDHeader, id)
			next.ServeHTTP(w, r.WithContext(WithCorrelationID(r.Context(), id)))
		})
	}
}

// validCorrelationID accepts a UUID (v4 or v7). Anything else is
// dropped — defense against malformed/giant inbound headers being
// echoed back into logs + response headers. UUID parse is cheap +
// catches the obvious abuse cases without a heavier sanitiser.
func validCorrelationID(s string) bool {
	if s == "" {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}
