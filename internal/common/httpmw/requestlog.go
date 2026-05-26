package httpmw

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

// statusCapture wraps http.ResponseWriter to remember the status code
// the handler wrote. http.ResponseWriter doesn't expose Status()
// natively; the canonical workaround is a small interceptor.
//
// Default to 200 so handlers that write a body without an explicit
// WriteHeader still log as 200 (which is what stdlib actually
// returns).
type statusCapture struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusCapture) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapture) Write(b []byte) (int, error) {
	if s.status == 0 {
		// Implicit 200 — handler wrote body without calling WriteHeader.
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// RequestLog emits a structured slog line at request end with the
// canonical observability fields (per audit-checklist.md §12):
// method, path, status, duration, bytes, correlation_id, tenant_id
// (when bound by RequireAuth), remote_addr, user_agent.
//
// Order policy: sits between Correlation and Recover so:
//   - the log line carries the same correlation_id as inner logs
//     (Correlation runs outside it).
//   - panics in inner handlers are recovered before this middleware
//     reads s.status, so the recover-driven 500 is what gets logged
//     (not whatever the inner handler half-wrote before panicking).
//
// Level policy: Info on 2xx + 3xx, Warn on 4xx, Error on 5xx. This
// matches the slog convention where Error means "operator should
// investigate" and 5xx is the only class that always warrants that.
// 4xx is client error — too noisy for Error in normal operation,
// too important to bury at Debug.
func RequestLog(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		panic("httpmw: RequestLog log required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			capture := &statusCapture{ResponseWriter: w}
			next.ServeHTTP(capture, r)
			latency := time.Since(start)

			tenantID, _ := tenancy.FromContext(r.Context())

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", capture.status,
				"duration_ms", latency.Milliseconds(),
				"bytes", capture.bytes,
				"correlation_id", CorrelationIDFromContext(r.Context()),
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			}
			if tenantID != "" {
				attrs = append(attrs, "tenant_id", tenantID.String())
			}

			switch {
			case capture.status >= 500:
				log.ErrorContext(r.Context(), "http request", attrs...)
			case capture.status >= 400:
				log.WarnContext(r.Context(), "http request", attrs...)
			default:
				log.InfoContext(r.Context(), "http request", attrs...)
			}
		})
	}
}
