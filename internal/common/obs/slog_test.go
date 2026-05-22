package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/obs"
)

// safeBuffer wraps bytes.Buffer for race-safe slog writes — slog
// handlers don't synchronise their underlying io.Writer.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestFanOutHandler_RoutesToBothSinks(t *testing.T) {
	t.Parallel()
	buf1, buf2 := &safeBuffer{}, &safeBuffer{}
	h := obs.FanOutHandler(
		slog.NewJSONHandler(buf1, &slog.HandlerOptions{Level: slog.LevelDebug}),
		slog.NewJSONHandler(buf2, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	logger := slog.New(h)
	logger.InfoContext(t.Context(), "shared message", "k", "v")

	for i, sink := range []*safeBuffer{buf1, buf2} {
		out := sink.String()
		if !strings.Contains(out, "shared message") {
			t.Fatalf("sink %d missing message: %q", i, out)
		}
		if !strings.Contains(out, `"k":"v"`) {
			t.Fatalf("sink %d missing attribute: %q", i, out)
		}
	}
}

func TestFanOutHandler_NilHandlersIgnored(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	h := obs.FanOutHandler(
		nil,
		slog.NewJSONHandler(buf, nil),
		nil,
	)
	logger := slog.New(h)
	logger.InfoContext(t.Context(), "ok")
	if !strings.Contains(buf.String(), `"msg":"ok"`) {
		t.Fatalf("non-nil sink missing message: %q", buf.String())
	}
}

func TestFanOutHandler_WithAttrsPropagates(t *testing.T) {
	t.Parallel()
	buf1, buf2 := &safeBuffer{}, &safeBuffer{}
	base := obs.FanOutHandler(
		slog.NewJSONHandler(buf1, nil),
		slog.NewJSONHandler(buf2, nil),
	)
	enriched := base.WithAttrs([]slog.Attr{slog.String("module", "identity")})

	logger := slog.New(enriched)
	logger.InfoContext(t.Context(), "msg")

	for i, sink := range []*safeBuffer{buf1, buf2} {
		var rec map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(sink.String())), &rec); err != nil {
			t.Fatalf("sink %d: decode: %v", i, err)
		}
		if rec["module"] != "identity" {
			t.Fatalf("sink %d: module attr missing: %+v", i, rec)
		}
	}
}

func TestFanOutHandler_WithGroupPropagates(t *testing.T) {
	t.Parallel()
	buf1, buf2 := &safeBuffer{}, &safeBuffer{}
	base := obs.FanOutHandler(
		slog.NewJSONHandler(buf1, nil),
		slog.NewJSONHandler(buf2, nil),
	)
	grouped := base.WithGroup("svc")
	logger := slog.New(grouped)
	logger.InfoContext(t.Context(), "msg", "k", "v")

	for i, sink := range []*safeBuffer{buf1, buf2} {
		out := sink.String()
		if !strings.Contains(out, `"svc":{"k":"v"}`) {
			t.Fatalf("sink %d: group not applied: %q", i, out)
		}
	}
}

func TestFanOutHandler_EnabledORsChildren(t *testing.T) {
	t.Parallel()
	buf := &safeBuffer{}
	h := obs.FanOutHandler(
		slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelError}),
		slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Enabled(INFO) should be true (second child accepts Debug+)")
	}
}
