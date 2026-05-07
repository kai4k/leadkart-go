// Package email defines the [Gateway] interface — the abstraction
// every Identity flow uses to deliver emails (password-reset, email-
// change confirmation, welcome, security alerts).
//
// The interface lives at the platform tier because every module that
// needs outbound email plugs into the same gateway. Production wires
// a provider-backed implementation (Msg91 / SES / SendGrid / Postmark);
// tests + dev wire a [Recorder] that captures calls for assertion.
//
// Per LeadKart .NET parent's IEmailGateway + the messaging.md
// "Cascading messages" canon: the application command handler emits
// the integration event AND publishes through this gateway. The
// gateway is NOT the source of truth — failed deliveries surface as
// errors and the caller decides whether to retry / dead-letter /
// roll back.
//
// Authentic 2026 .NET-parity: Strategy pattern keyed by provider
// (Msg91/SES/SendGrid/...) — see `design-patterns.md` "Strategy" +
// `pattern-selection-guide.md`. Provider selection is a wiring-time
// choice in the composition root, not a per-request decision.
package email

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
)

// ErrInvalid is returned by validation failures inside the gateway
// surface. Wrapped per the standard go errors.Is convention.
var ErrInvalid = errors.New("email: invalid")

// ErrSend is returned when the underlying provider call fails.
// Distinct from ErrInvalid so callers can decide whether to retry
// (transient) vs surface a 4xx (validation).
var ErrSend = errors.New("email: send failed")

// Message is the outbound-email value object the gateway sends. The
// VO is validated at construction — bad data fails the boundary, not
// the provider.
type Message struct {
	to       email.Address
	from     email.Address
	subject  string
	bodyText string
	bodyHTML string
	tenantID string // optional — propagates per-tenant branding to the provider
}

// MessageOption is the canonical functional-options shape for [Message]
// construction. Mat Ryer 2024 canon uses positional ctors for HTTP servers,
// but the email-message surface has many optional fields (HTML body,
// tenant context, future cc/bcc) — options reads cleanly here.
type MessageOption func(*Message)

// WithHTMLBody adds an HTML alternative body alongside the plain-text
// fallback. Per RFC 2046 multipart/alternative — providers send both
// and the client picks based on capability.
func WithHTMLBody(html string) MessageOption {
	return func(m *Message) { m.bodyHTML = html }
}

// WithTenantID tags the message with a tenant identifier so the
// provider can apply tenant-scoped branding (logo, sender name,
// reply-to) at delivery time. Empty string disables the tag.
func WithTenantID(tenantID string) MessageOption {
	return func(m *Message) { m.tenantID = tenantID }
}

// NewMessage constructs an outbound-email Message. Validates required
// fields + applies options.
func NewMessage(to, from email.Address, subject, bodyText string, opts ...MessageOption) (Message, error) {
	if to.IsZero() {
		return Message{}, fmt.Errorf("%w: to address required", ErrInvalid)
	}
	if from.IsZero() {
		return Message{}, fmt.Errorf("%w: from address required", ErrInvalid)
	}
	if strings.TrimSpace(subject) == "" {
		return Message{}, fmt.Errorf("%w: subject required", ErrInvalid)
	}
	if strings.TrimSpace(bodyText) == "" {
		return Message{}, fmt.Errorf("%w: bodyText required (HTML alone is not enough — fallback for non-HTML clients per RFC 2046)", ErrInvalid)
	}
	m := Message{
		to:       to,
		from:     from,
		subject:  subject,
		bodyText: bodyText,
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m, nil
}

// To returns the recipient address.
func (m Message) To() email.Address { return m.to }

// From returns the sender address.
func (m Message) From() email.Address { return m.from }

// Subject returns the email subject.
func (m Message) Subject() string { return m.subject }

// BodyText returns the plain-text body (always present per RFC 2046).
func (m Message) BodyText() string { return m.bodyText }

// BodyHTML returns the HTML body, or empty string if none was supplied.
func (m Message) BodyHTML() string { return m.bodyHTML }

// HasHTMLBody reports whether the message includes an HTML alternative.
func (m Message) HasHTMLBody() bool { return m.bodyHTML != "" }

// TenantID returns the tenant context tag, or empty if not set.
func (m Message) TenantID() string { return m.tenantID }

// Gateway is the canonical outbound-email port. Implementations live
// in `internal/platform/email/{provider}/`; the composition root in
// `cmd/api/main.go` wires the production provider and tests use the
// Recorder defined below.
//
// Send is synchronous — the caller decides queuing semantics. Per
// security.md "Login flow" + audit-checklist.md §13: outbound email
// is a delivery concern, not an audit-log concern. The audit row
// records "user requested password reset"; the delivery success/
// failure does NOT need to be in the audit log because it can be
// re-delivered idempotently from the source-of-truth event.
type Gateway interface {
	Send(ctx context.Context, m Message) error
}

// ----- Recorder ------------------------------------------------------

// Recorder is the test-double Gateway. Stores every Send call in
// FIFO order so tests can assert "was this email sent? to whom?
// with what subject?" without spinning a real provider.
//
// Concurrent-safe: Recorder is intended for table-driven tests that
// run with t.Parallel — the internal mutex guards the slice.
type Recorder struct {
	sent []Message
	mu   sync.Mutex
	now  func() time.Time
}

// NewRecorder constructs a Recorder. Pass nil for `now` to use
// time.Now (Recorder doesn't currently stamp messages, but a future
// audit-trail extension may).
func NewRecorder(now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{now: now}
}

// Send captures the message. Always returns nil (no provider failure
// simulation today; add WithFailMode-style options if a test needs
// to assert error paths).
func (r *Recorder) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return nil
}

// Sent returns a copy of every captured message in FIFO order.
func (r *Recorder) Sent() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Message, len(r.sent))
	copy(out, r.sent)
	return out
}

// Count returns the number of messages captured. Cheaper than
// `len(r.Sent())` when the test only wants the count.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

// Reset drops all captured messages. Use in test setup when the
// Recorder is shared across multiple sub-tests.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = nil
}
