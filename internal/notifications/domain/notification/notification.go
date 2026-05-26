// Package notification owns the [Notification] aggregate — one row
// per cross-module user-facing alert in `notifications.notifications`.
// Per BRD §6.9 + ADR 0063 §4 (subscriber-decides pattern).
//
// Lifecycle:
//
//	unread → read    (terminal-success — the recipient saw it)
//	      ↘ dismissed (terminal-success-no-action — the recipient closed
//	                    without acting on it)
//
// Append-mostly: rows are written once + transition state at most
// twice (unread → read, optionally → dismissed). Soft-delete via
// purge cron (read after 7 days, unread after 30 days) per BRD.
package notification

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrInvalid is the sentinel for ctor / mutator invariant violations.
// Map to HTTP 422.
var ErrInvalid = errors.New("notification: invalid")

// ErrInvalidTransition — illegal (cur, target) state edge. Map to 409.
var ErrInvalidTransition = errors.New("notification: invalid state transition")

// ID is a UUIDv7.
type ID string

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id == "" }

// String returns the underlying UUID string.
func (id ID) String() string { return string(id) }

// State is the lifecycle position of the Notification.
type State string

// Closed catalogue.
const (
	StateUnread    State = "unread"
	StateRead      State = "read"
	StateDismissed State = "dismissed"
)

// String returns the wire form.
func (s State) String() string { return string(s) }

// IsTerminal reports whether the state allows NO further transitions.
func (s State) IsTerminal() bool { return s == StateRead || s == StateDismissed }

// IsValid reports whether s is in the catalogue.
func (s State) IsValid() bool {
	switch s {
	case StateUnread, StateRead, StateDismissed:
		return true
	}
	return false
}

// ParseState turns an untrusted string into a State or returns
// [ErrInvalid].
func ParseState(raw string) (State, error) {
	s := State(raw)
	if !s.IsValid() {
		return "", fmt.Errorf("%w: state %q not in catalogue", ErrInvalid, raw)
	}
	return s, nil
}

// Notification is the aggregate root.
type Notification struct {
	id                   ID
	tenantID             tenant.ID
	recipientMembershipID membership.ID
	category             Category
	title                string
	body                 string
	state                State

	// Source-entity tracking (dedup key + click-through deep-link).
	// SourceModule + SourceEntityType + SourceEntityID identify the
	// "thing" this notification is about — used by the partial unique
	// index for the 5-minute dedup window.
	sourceModule     string
	sourceEntityType string
	sourceEntityID   string

	// Optional URL the recipient is sent to when they click the
	// notification — relative path the BFF resolves against its origin.
	deepLink string

	createdAt   time.Time
	readAt      *time.Time
	dismissedAt *time.Time

	events []Event
}

// NewInput is the ctor input. Sourced almost exclusively by
// subscribers — manual creation is rare but supported via HTTP.
type NewInput struct {
	ID                    ID
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
	Category              Category
	Title                 string
	Body                  string
	SourceModule          string
	SourceEntityType      string
	SourceEntityID        string
	DeepLink              string
	Now                   time.Time
}

// New constructs an unread Notification. Title is required; Body is
// optional (some notifications are title-only).
func New(in NewInput) (*Notification, error) {
	if in.ID.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if in.TenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalid)
	}
	if in.RecipientMembershipID == "" {
		return nil, fmt.Errorf("%w: recipient_membership_id required", ErrInvalid)
	}
	if !in.Category.IsValid() {
		return nil, fmt.Errorf("%w: category %q not in catalogue", ErrInvalid, in.Category)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, fmt.Errorf("%w: title required", ErrInvalid)
	}
	if len(title) > 200 {
		return nil, fmt.Errorf("%w: title max 200 chars (got %d)", ErrInvalid, len(title))
	}
	if in.Now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	// Source fields are an all-or-nothing tuple: either all three are
	// set (dedup applies, deep-link resolves) or all three are empty
	// (manual notification, no source row).
	allSet := in.SourceModule != "" && in.SourceEntityType != "" && in.SourceEntityID != ""
	allEmpty := in.SourceModule == "" && in.SourceEntityType == "" && in.SourceEntityID == ""
	if !allSet && !allEmpty {
		return nil, fmt.Errorf("%w: source_module/type/id must all be set or all empty", ErrInvalid)
	}
	n := &Notification{
		id:                    in.ID,
		tenantID:              in.TenantID,
		recipientMembershipID: in.RecipientMembershipID,
		category:              in.Category,
		title:                 title,
		body:                  strings.TrimSpace(in.Body),
		state:                 StateUnread,
		sourceModule:          in.SourceModule,
		sourceEntityType:      in.SourceEntityType,
		sourceEntityID:        in.SourceEntityID,
		deepLink:              strings.TrimSpace(in.DeepLink),
		createdAt:             in.Now,
	}
	n.recordEvent(CreatedEvent{
		NotificationID:        n.id,
		TenantID:              n.tenantID,
		RecipientMembershipID: n.recipientMembershipID,
		Category:              n.category,
		SourceModule:          n.sourceModule,
		SourceEntityType:      n.sourceEntityType,
		SourceEntityID:        n.sourceEntityID,
		CreatedAt:             n.createdAt,
	})
	return n, nil
}

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                    ID
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
	Category              Category
	Title                 string
	Body                  string
	State                 State
	SourceModule          string
	SourceEntityType      string
	SourceEntityID        string
	DeepLink              string
	CreatedAt             time.Time
	ReadAt                *time.Time
	DismissedAt           *time.Time
}

// UnmarshalFromDB rehydrates without re-validating.
func UnmarshalFromDB(s Snapshot) *Notification {
	return &Notification{
		id:                    s.ID,
		tenantID:              s.TenantID,
		recipientMembershipID: s.RecipientMembershipID,
		category:              s.Category,
		title:                 s.Title,
		body:                  s.Body,
		state:                 s.State,
		sourceModule:          s.SourceModule,
		sourceEntityType:      s.SourceEntityType,
		sourceEntityID:        s.SourceEntityID,
		deepLink:              s.DeepLink,
		createdAt:             s.CreatedAt,
		readAt:                s.ReadAt,
		dismissedAt:           s.DismissedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the aggregate identity.
func (n *Notification) ID() ID { return n.id }

// TenantID returns the owning tenant.
func (n *Notification) TenantID() tenant.ID { return n.tenantID }

// RecipientMembershipID returns the addressed membership.
func (n *Notification) RecipientMembershipID() membership.ID { return n.recipientMembershipID }

// Category returns the source-domain tag.
func (n *Notification) Category() Category { return n.category }

// Title returns the display title.
func (n *Notification) Title() string { return n.title }

// Body returns the display body (may be empty).
func (n *Notification) Body() string { return n.body }

// State returns the current lifecycle state.
func (n *Notification) State() State { return n.state }

// SourceModule returns the source-bounded-context name (empty for
// manual notifications).
func (n *Notification) SourceModule() string { return n.sourceModule }

// SourceEntityType returns the source-row table-name (empty for manual).
func (n *Notification) SourceEntityType() string { return n.sourceEntityType }

// SourceEntityID returns the source-row primary key (empty for manual).
func (n *Notification) SourceEntityID() string { return n.sourceEntityID }

// DeepLink returns the click-through path.
func (n *Notification) DeepLink() string { return n.deepLink }

// CreatedAt returns the row-creation timestamp.
func (n *Notification) CreatedAt() time.Time { return n.createdAt }

// ReadAt returns the read timestamp or nil.
func (n *Notification) ReadAt() *time.Time { return n.readAt }

// DismissedAt returns the dismissal timestamp or nil.
func (n *Notification) DismissedAt() *time.Time { return n.dismissedAt }

// ----- State transitions ----------------------------------------------------

// MarkRead flips unread → read. Idempotent on self. Rejected against
// dismissed (terminal-failure-of-different-kind).
func (n *Notification) MarkRead(now time.Time) error {
	if n.state == StateRead {
		return nil // idempotent
	}
	if n.state == StateDismissed {
		return fmt.Errorf("%w: cannot mark dismissed notification as read", ErrInvalidTransition)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	n.state = StateRead
	n.readAt = &now
	n.recordEvent(MarkedReadEvent{
		NotificationID:        n.id,
		TenantID:              n.tenantID,
		RecipientMembershipID: n.recipientMembershipID,
		ReadAt:                now,
	})
	return nil
}

// Dismiss flips any non-terminal state → dismissed. Per the UX:
// unread → dismissed (recipient closed without reading) AND read →
// dismissed (recipient is done with it). Idempotent on self.
func (n *Notification) Dismiss(now time.Time) error {
	if n.state == StateDismissed {
		return nil
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	prior := n.state
	n.state = StateDismissed
	n.dismissedAt = &now
	n.recordEvent(DismissedEvent{
		NotificationID:        n.id,
		TenantID:              n.tenantID,
		RecipientMembershipID: n.recipientMembershipID,
		PriorState:            prior,
		DismissedAt:           now,
	})
	return nil
}

// ----- Events --------------------------------------------------------------

// PullEvents drains the recorded domain events.
func (n *Notification) PullEvents() []Event {
	if len(n.events) == 0 {
		return nil
	}
	out := n.events
	n.events = nil
	return out
}

func (n *Notification) recordEvent(e Event) { n.events = append(n.events, e) }
