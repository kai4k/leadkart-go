package httpmw

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// errCodeInternal is the wire-stable error code for unrecovered
// panics. Distinct from any per-handler error code so an attacker
// can't probe "did the handler reach an internal error" vs. "did it
// panic" — both surface the same shape.
const errCodeInternal = "internal_error"

// Recover catches panics from inner handlers, logs them with the
// request's correlation ID + stack trace, and returns a generic
// 500. Without this, a panic in any handler crashes the entire
// goroutine pool — the http.Server in stdlib catches panics per-
// request, but the result is a connection close with no body, no
// log line, and no telemetry. This gives operators a structured
// log line + a clean error response.
//
// Order policy: Recover sits BELOW correlation + requestlog so the
// panic log line carries the same correlation_id as the rest of
// the request's logs and so the request-end log line records the
// 500 status the recover middleware writes.
//
// http.ErrAbortHandler is propagated unchanged per stdlib canon —
// it's the documented escape hatch for handlers that want to
// abort without logging (e.g. timeout-driven aborts upstream).
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// Re-panic on the stdlib's reserved abort sentinel so the
				// http.Server's own recovery path kicks in. Per net/http
				// docs: "ErrAbortHandler is a sentinel panic value to
				// abort a handler. While any panic from ServeHTTP aborts
				// the response to the client, panicking with
				// ErrAbortHandler also suppresses logging of a stack
				// trace to the server's error log."
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic recovered",
					"panic", rec,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
					"correlation_id", CorrelationIDFromContext(r.Context()),
				)
				// Best-effort 500. If the inner handler already wrote
				// headers, this is a no-op; the connection still
				// terminates cleanly because the stdlib closes after
				// ServeHTTP returns.
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"` + errCodeInternal + `","message":"internal server error"}`))
			}()
			next.ServeHTTP(w, r)
		})
	}
}
