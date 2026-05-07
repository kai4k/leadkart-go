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

// ProfileUpdatedEvent fires when [Person.UpdateProfile] is called.
//
// Carries the OLD + NEW values so audit subscribers can render
// "Alice Sharma → Alice Sharma-Khan" diffs without re-hydrating the
// aggregate. Per .NET parent's MembershipProfileUpdatedEvent shape
// (with Person fields here, not Membership-tenant-scoped fields).
type ProfileUpdatedEvent struct {
	PersonID     ID
	OldFirstName string
	OldLastName  string
	NewFirstName string
	NewLastName  string
	At           time.Time
}

// Topic returns the integration-event type.
func (ProfileUpdatedEvent) Topic() string { return "identity.person_profile_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e ProfileUpdatedEvent) OccurredAt() time.Time { return e.At }

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

// EmailChangeRequestedEvent fires when [Person.RequestEmailChange]
// is called. Carries the proposed new email + expiry — but NOT the
// raw token (raw leaves via the application-layer integration event
// for delivery to the new address).
//
// Per Auth0 / Stripe / Microsoft Entra ID canon: the most recent
// request supersedes any prior pending email change. The previous
// emailed token is silently invalidated.
type EmailChangeRequestedEvent struct {
	PersonID  ID
	NewEmail  email.Address
	ExpiresAt time.Time
	At        time.Time
}

// Topic returns the integration-event type.
func (EmailChangeRequestedEvent) Topic() string { return "identity.email_change_requested.v1" }

// OccurredAt returns the domain timestamp.
func (e EmailChangeRequestedEvent) OccurredAt() time.Time { return e.At }

// EmailChangedEvent fires when [Person.ConfirmEmailChange] successfully
// applies a new email via a valid confirmation token.
//
// SecurityStamp rotates per security.md "SecurityStamp rotation
// triggers" — email changes invalidate outstanding JWTs because the
// `sub` identity is logically rebound (an attacker who hijacked a
// session can't keep operating after the legitimate user rotates
// the address).
//
// Cross-module subscribers (Notifications: confirm to old address)
// receive both old + new for audit.
type EmailChangedEvent struct {
	PersonID ID
	OldEmail email.Address
	NewEmail email.Address
	At       time.Time
}

// Topic returns the integration-event type.
func (EmailChangedEvent) Topic() string { return "identity.email_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e EmailChangedEvent) OccurredAt() time.Time { return e.At }

// EmailChangeCancelledEvent fires when [Person.CancelEmailChange]
// invalidates a pending change without applying it (admin action,
// security incident, or implicit cancel via direct admin email
// update).
type EmailChangeCancelledEvent struct {
	PersonID ID
	Reason   string
	At       time.Time
}

// Topic returns the integration-event type.
func (EmailChangeCancelledEvent) Topic() string { return "identity.email_change_cancelled.v1" }

// OccurredAt returns the domain timestamp.
func (e EmailChangeCancelledEvent) OccurredAt() time.Time { return e.At }

// GloballySuspendedEvent fires when [Person.GloballySuspend] is called.
//
// Rare — compliance / fraud / cross-tenant abuse. Subscribers (auth,
// every module's session blacklist) MUST treat the Person as
// completely banned: kill all refresh-token families, block login,
// reject all authenticated calls.
type GloballySuspendedEvent struct {
	PersonID ID
	Reason   string
	At       time.Time
}

// Topic returns the integration-event type.
func (GloballySuspendedEvent) Topic() string { return "identity.person_globally_suspended.v1" }

// OccurredAt returns the domain timestamp.
func (e GloballySuspendedEvent) OccurredAt() time.Time { return e.At }

// PasswordResetRequestedEvent fires when [Person.RequestPasswordReset]
// is called. Carries the expiry timestamp for downstream subscribers
// (audit, SIEM) but NOT the raw token — the raw leaves the domain via
// the application-layer integration event for email delivery only.
//
// Per security.md "Password reset" + Auth0 / Okta canon: the most
// recent request supersedes any prior pending reset. Subscribers MAY
// log the supersede for forensics.
type PasswordResetRequestedEvent struct {
	PersonID  ID
	ExpiresAt time.Time
	At        time.Time
}

// Topic returns the integration-event type.
func (PasswordResetRequestedEvent) Topic() string { return "identity.password_reset_requested.v1" }

// OccurredAt returns the domain timestamp.
func (e PasswordResetRequestedEvent) OccurredAt() time.Time { return e.At }

// PasswordResetConfirmedEvent fires when [Person.ConfirmPasswordReset]
// successfully applies a new password via a valid reset token.
//
// Distinct from PasswordChangedEvent (which fires for any password
// change — including admin reset, change-password-while-logged-in,
// AND token-based reset). This narrower event lets subscribers
// distinguish "user reset their own password via emailed token" from
// "admin reset" for compliance reporting.
type PasswordResetConfirmedEvent struct {
	PersonID ID
	At       time.Time
}

// Topic returns the integration-event type.
func (PasswordResetConfirmedEvent) Topic() string { return "identity.password_reset_confirmed.v1" }

// OccurredAt returns the domain timestamp.
func (e PasswordResetConfirmedEvent) OccurredAt() time.Time { return e.At }

// PasswordResetCancelledEvent fires when [Person.CancelPasswordReset]
// invalidates a pending reset (admin action, security-incident
// response, or implicit cancel via direct password change while a
// reset was pending).
type PasswordResetCancelledEvent struct {
	PersonID ID
	Reason   string
	At       time.Time
}

// Topic returns the integration-event type.
func (PasswordResetCancelledEvent) Topic() string { return "identity.password_reset_cancelled.v1" }

// OccurredAt returns the domain timestamp.
func (e PasswordResetCancelledEvent) OccurredAt() time.Time { return e.At }

// GlobalSuspensionLiftedEvent fires when [Person.LiftGlobalSuspension]
// reverses a previous global suspension. Subscribers re-enable login
// + remove the SIEM block.
type GlobalSuspensionLiftedEvent struct {
	PersonID ID
	At       time.Time
}

// Topic returns the integration-event type.
func (GlobalSuspensionLiftedEvent) Topic() string {
	return "identity.person_global_suspension_lifted.v1"
}

// OccurredAt returns the domain timestamp.
func (e GlobalSuspensionLiftedEvent) OccurredAt() time.Time { return e.At }
