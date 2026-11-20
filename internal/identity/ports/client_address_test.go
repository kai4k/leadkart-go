package ports

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

// The device label falls back to the ORIGINATING client address, never the
// socket peer (which behind the BFF is the BFF itself).
func TestResolveDeviceLabel_FallsBackToTheClientAddressNotThePeer(t *testing.T) {
	t.Parallel()
	trusted, err := httpmw.ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	var label string
	h := httpmw.ClientAddress(slog.New(slog.NewTextHandler(io.Discard, nil)), trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		label = resolveDeviceLabel("", r)
	}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", nil)
	r.RemoteAddr = "10.0.0.5:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	r.Header.Del("User-Agent")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if label != "198.51.100.7" {
		t.Fatalf("label = %q, want the forwarded client 198.51.100.7", label)
	}
	// A request that never passed the chain has no client address: not the peer.
	bare := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	bare.RemoteAddr = "10.0.0.5:4000"
	if got := resolveDeviceLabel("", bare); got != "Unknown device" {
		t.Fatalf("label without the chain = %q, want Unknown device", got)
	}
}

// Retry-After is rounded UP: 1.2 s left must advertise 2, not 1.
func TestWriteLockoutError_RetryAfterIsRoundedUp(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	writeLockoutErrorAt(w, now.Add(90*time.Second+500*time.Millisecond), now)
	got, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After %q: %v", w.Header().Get("Retry-After"), err)
	}
	if got != 91 { // 90.5 s left rounds up to 91 (truncation said 90)
		t.Fatalf("Retry-After = %d, want 91 (rounded up from 90.5)", got)
	}
	if w.Code != http.StatusLocked {
		t.Fatalf("status = %d", w.Code)
	}
}
