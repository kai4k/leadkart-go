package email_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	emailaddr "github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/platform/email"
)

func mustAddr(t *testing.T, raw string) emailaddr.Address {
	t.Helper()
	a, err := emailaddr.New(raw)
	if err != nil {
		t.Fatalf("emailaddr.New(%q): %v", raw, err)
	}
	return a
}

func TestNewMessage_AcceptsValid(t *testing.T) {
	t.Parallel()
	m, err := email.NewMessage(
		mustAddr(t, "alice@acme.test"),
		mustAddr(t, "noreply@leadkart.io"),
		"Reset your password",
		"Click the link to reset your password.",
	)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if m.To().String() != "alice@acme.test" {
		t.Errorf("To = %q", m.To())
	}
	if m.Subject() != "Reset your password" {
		t.Errorf("Subject = %q", m.Subject())
	}
	if m.HasHTMLBody() {
		t.Error("HasHTMLBody should be false without WithHTMLBody")
	}
}

func TestNewMessage_AcceptsOptions(t *testing.T) {
	t.Parallel()
	m, err := email.NewMessage(
		mustAddr(t, "alice@acme.test"),
		mustAddr(t, "noreply@leadkart.io"),
		"Reset your password",
		"Click the link to reset your password.",
		email.WithHTMLBody("<p>Click <a>here</a></p>"),
		email.WithTenantID("tenant-acme-123"),
	)
	if err != nil {
		t.Fatalf("NewMessage with options: %v", err)
	}
	if !m.HasHTMLBody() || m.BodyHTML() == "" {
		t.Error("HTML body not stored")
	}
	if m.TenantID() != "tenant-acme-123" {
		t.Errorf("TenantID = %q", m.TenantID())
	}
}

func TestNewMessage_RejectsInvalid(t *testing.T) {
	t.Parallel()
	to := mustAddr(t, "alice@acme.test")
	from := mustAddr(t, "noreply@leadkart.io")
	cases := []struct {
		name              string
		to, from          emailaddr.Address
		subject, bodyText string
	}{
		{"zero to", emailaddr.Address{}, from, "S", "B"},
		{"zero from", to, emailaddr.Address{}, "S", "B"},
		{"empty subject", to, from, "", "B"},
		{"whitespace subject", to, from, "   ", "B"},
		{"empty bodyText", to, from, "S", ""},
		{"whitespace bodyText", to, from, "S", "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := email.NewMessage(tc.to, tc.from, tc.subject, tc.bodyText)
			if !errors.Is(err, email.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestRecorder_CapturesSentMessages(t *testing.T) {
	t.Parallel()
	r := email.NewRecorder(nil)
	if r.Count() != 0 {
		t.Errorf("fresh Recorder Count = %d", r.Count())
	}

	m1, _ := email.NewMessage(
		mustAddr(t, "a@b.io"),
		mustAddr(t, "noreply@leadkart.io"),
		"S1", "B1",
	)
	m2, _ := email.NewMessage(
		mustAddr(t, "c@d.io"),
		mustAddr(t, "noreply@leadkart.io"),
		"S2", "B2",
	)
	if err := r.Send(context.Background(), m1); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := r.Send(context.Background(), m2); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if r.Count() != 2 {
		t.Errorf("Count after 2 sends = %d", r.Count())
	}
	sent := r.Sent()
	if len(sent) != 2 {
		t.Fatalf("Sent() = %d", len(sent))
	}
	if sent[0].Subject() != "S1" || sent[1].Subject() != "S2" {
		t.Errorf("FIFO order broken")
	}
}

func TestRecorder_Reset_ClearsCapture(t *testing.T) {
	t.Parallel()
	r := email.NewRecorder(nil)
	m, _ := email.NewMessage(
		mustAddr(t, "a@b.io"),
		mustAddr(t, "noreply@leadkart.io"),
		"S", "B",
	)
	_ = r.Send(context.Background(), m)
	if r.Count() != 1 {
		t.Fatal("expected 1 captured before reset")
	}
	r.Reset()
	if r.Count() != 0 {
		t.Errorf("Count after Reset = %d", r.Count())
	}
}

func TestRecorder_ConcurrentSafe(t *testing.T) {
	// Recorder is intended for t.Parallel — verify the mutex actually
	// prevents data races by hammering Send from multiple goroutines.
	t.Parallel()
	r := email.NewRecorder(nil)
	from := mustAddr(t, "noreply@leadkart.io")

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			m, _ := email.NewMessage(
				mustAddr(t, "a@b.io"),
				from,
				"S", "B",
			)
			_ = r.Send(context.Background(), m)
		})
	}
	wg.Wait()
	if r.Count() != 50 {
		t.Errorf("Count after concurrent sends = %d, want 50", r.Count())
	}
}

// Compile-time assertion that *Recorder satisfies email.Gateway —
// drift between the test double and the contract surfaces at build
// time, not at first runtime use.
var _ email.Gateway = (*email.Recorder)(nil)
