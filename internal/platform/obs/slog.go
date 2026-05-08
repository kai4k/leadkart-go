package obs

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// fanOutHandler dispatches every slog Record to multiple downstream
// handlers. Used to tee logs to a stdout JSON handler (for sidecar
// scrapers + local dev) AND otelslog (for OTLP-native log pipelines)
// without forcing the application to pick one.
//
// Stdlib log/slog has no MultiHandler; the conventional shape is a
// small fan-out struct. Implementation matches the slog.Handler
// godoc contract: each downstream handler is independent — Enabled
// returns OR-of-children, Handle propagates errors via errors.Join,
// WithAttrs/WithGroup deep-copy the children.
type fanOutHandler struct {
	handlers []slog.Handler
}

// FanOutHandler returns a slog.Handler that fans every Record out
// to each child. Used by the binaries' main() to tee logs to stdout
// + OTLP.
func FanOutHandler(handlers ...slog.Handler) slog.Handler {
	cleaned := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			cleaned = append(cleaned, h)
		}
	}
	return &fanOutHandler{handlers: cleaned}
}

func (f *fanOutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f *fanOutHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		// Per slog.Handler godoc: handlers should not modify the
		// Record. Each child clones via record.Clone() if it needs
		// to mutate; Handle here just passes the same Record through.
		if err := h.Handle(ctx, record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (f *fanOutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return f
	}
	cloned := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		cloned[i] = h.WithAttrs(attrs)
	}
	return &fanOutHandler{handlers: cloned}
}

func (f *fanOutHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return f
	}
	cloned := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		cloned[i] = h.WithGroup(name)
	}
	return &fanOutHandler{handlers: cloned}
}

// NewSlogHandler returns the canonical multi-target slog.Handler used
// by both cmd/api and cmd/worker:
//
//   - stdoutHandler renders structured JSON to stdout (consumed by
//     local dev tail-f, container log driver, sidecar scrapers).
//   - otelslog bridges the same record into the global LoggerProvider
//     installed by [Setup] — production OTLP backend (Loki / Tempo /
//     dedicated log store) sees logs natively with trace context.
//
// serviceName is the OTel resource service.name — typically the
// binary name ("leadkart-api" / "leadkart-worker"). otelslog uses it
// as the logger Name so backends can group records by source binary.
func NewSlogHandler(stdoutHandler slog.Handler, serviceName string) slog.Handler {
	return FanOutHandler(stdoutHandler, otelslog.NewHandler(serviceName))
}
