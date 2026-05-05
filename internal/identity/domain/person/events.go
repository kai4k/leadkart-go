package person

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
)

// Event is the marker interface for Person domain events.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// CreatedEvent fires when a new Person is created via [New].
type CreatedEvent struct {
	PersonID  ID
	Email     email.Address
	FirstName string
	LastName  string
	At        time.Time
}

// Topic returns the integration-event type.
func (CreatedEvent) Topic() string { return "identity.person_created.v1" }

// OccurredAt returns the domain timestamp.
func (e CreatedEvent) OccurredAt() time.Time { return e.At }

// PasswordChangedEvent fires when [Person.ChangePassword] is called.
//
// Subscribers (auth, audit, notifications) react to invalidate sessions
// + send "your password was changed" emails per `security.md` canon.
type PasswordChangedEvent struct {
	PersonID ID
	At       time.Time
}

// Topic returns the integration-event type.
func (PasswordChangedEvent) Topic() string { return "identity.password_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e PasswordChangedEvent) OccurredAt() time.Time { return e.At }

// AnonymisedEvent fires when [Person.Anonymise] is called.
//
// Cross-module subscribers MUST scrub their own PII (CRM lead notes,
// Tasks comments, audit-row author labels, etc.) per DPDP / GDPR.
type AnonymisedEvent struct {
	PersonID ID
	At       time.Time
}

// Topic returns the integration-event type.
func (AnonymisedEvent) Topic() string { return "identity.person_anonymised.v1" }

// OccurredAt returns the domain timestamp.
func (e AnonymisedEvent) OccurredAt() time.Time { return e.At }
