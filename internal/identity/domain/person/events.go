package person

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
)

// Event is the SEALED marker interface for Person domain events.
// Sealed via the unexported isPersonEvent() method so only types in
// this package can satisfy it — same shape as role.Event.
//
// Domain events deliberately do NOT carry wire concerns (Topic / V1
// alias / occurred-at-as-method). Wire-versioning lives in
// integrationevents.*V1 per Vernon IDDD ch. 8 ("Domain Events vs.
// Integration Events"): a v2 wire rename must NOT force a domain edit.
// The integration mapper in internal/identity/integrationevents/
// type-switches on these structs and emits the canonical V1 envelope.
type Event interface {
	isPersonEvent()
}

// CreatedEvent fires when a new Person is created via [New].
type CreatedEvent struct {
	PersonID  ID
	Email     email.Address
	FirstName string
	LastName  string
	At        time.Time
}

func (CreatedEvent) isPersonEvent() {}

// PasswordChangedEvent fires when [Person.ChangePassword] is called.
//
// Subscribers (auth, audit, notifications) react to invalidate sessions
// + send "your password was changed" emails per `security.md` canon.
type PasswordChangedEvent struct {
	PersonID ID
	At       time.Time
}

func (PasswordChangedEvent) isPersonEvent() {}

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

func (ProfileUpdatedEvent) isPersonEvent() {}

// AnonymisedEvent fires when [Person.Anonymise] is called.
//
// Cross-module subscribers MUST scrub their own PII (CRM lead notes,
// Tasks comments, audit-row author labels, etc.) per DPDP / GDPR.
type AnonymisedEvent struct {
	PersonID ID
	At       time.Time
}

func (AnonymisedEvent) isPersonEvent() {}

// EmailChangeRequestedEvent fires when [Person.RequestEmailChange]
// is called. Carries the proposed new email + expiry — the AUDIT-side
// signal of the change flow.
//
// Action-side delivery (the confirmation link emailed to the NEW
// address) rides on the SIBLING [EmailChangeConfirmationRequestedEvent]
// per ADR 0057 — same two-event split as the password-reset path.
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

func (EmailChangeRequestedEvent) isPersonEvent() {}

// EmailChangeConfirmationRequestedEvent is the ACTION-side counterpart
// to [EmailChangeRequestedEvent] per ADR 0057 — carries the plaintext
// confirmation token + new+old addresses so the email subscriber can
// build + deliver the confirmation link asynchronously.
//
// Confirmation goes to the NEW address (proves user controls the
// destination) per Auth0 / Okta canon. OldEmail is carried for the
// "informational notify to old" follow-up subscriber (currently
// deferred — see ADR 0057 §"Deferred work").
type EmailChangeConfirmationRequestedEvent struct {
	PersonID       ID
	NewEmail       email.Address
	OldEmail       email.Address
	PlaintextToken string
	ExpiresAt      time.Time
	RecipientName  string
	At             time.Time
}

func (EmailChangeConfirmationRequestedEvent) isPersonEvent() {}

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

func (EmailChangedEvent) isPersonEvent() {}

// EmailChangeCancelledEvent fires when [Person.CancelEmailChange]
// invalidates a pending change without applying it (admin action,
// security incident, or implicit cancel via direct admin email
// update).
type EmailChangeCancelledEvent struct {
	PersonID ID
	Reason   string
	At       time.Time
}

func (EmailChangeCancelledEvent) isPersonEvent() {}

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

func (GloballySuspendedEvent) isPersonEvent() {}

// PasswordResetRequestedEvent fires when [Person.RequestPasswordReset]
// is called. Carries the expiry timestamp for downstream subscribers
// (audit, SIEM) but NOT the raw token — the AUDIT-side signal of the
// reset flow.
//
// Action-side delivery (sending the email with the link) rides on the
// SIBLING [PasswordResetEmailRequestedEvent] per ADR 0057 — same
// trigger, two events, two consumers: audit reads this; the email
// subscriber reads the sibling.
//
// Per security.md "Password reset" + Auth0 / Okta canon: the most
// recent request supersedes any prior pending reset. Subscribers MAY
// log the supersede for forensics.
type PasswordResetRequestedEvent struct {
	PersonID  ID
	ExpiresAt time.Time
	At        time.Time
}

func (PasswordResetRequestedEvent) isPersonEvent() {}

// PasswordResetEmailRequestedEvent is the ACTION-side counterpart to
// [PasswordResetRequestedEvent] per ADR 0057 — fires from the same
// aggregate method but carries the plaintext token + recipient details
// so a Watermill subscriber can deliver the email asynchronously.
//
// Why two events at the same trigger:
//   - PasswordResetRequestedEvent  = AUDIT signal (no plaintext; broad
//                                    consumer set: SIEM, audit-log,
//                                    forensics).
//   - PasswordResetEmailRequestedEvent = ACTION signal (plaintext;
//     single consumer = email subscriber).
//
// Security boundary on the plaintext: outbox is RLS-scoped + tenant-
// isolated + the forwarder drains within ~1s, so the plaintext window
// at-rest is short (≤1s typical, ≤token TTL bound). Same boundary
// Stripe / Auth0 use for short-lived OTP delivery via async queues.
//
// Email is NOT delivered to non-existent persons (silent-success at
// the command handler — see security.md "Enumeration safety"); the
// aggregate only emits this event when a real Person exists.
type PasswordResetEmailRequestedEvent struct {
	PersonID       ID
	Email          email.Address
	PlaintextToken string
	ExpiresAt      time.Time
	RecipientName  string // FirstName at time of event — UI display, falls back to local-part
	At             time.Time
}

func (PasswordResetEmailRequestedEvent) isPersonEvent() {}

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

func (PasswordResetConfirmedEvent) isPersonEvent() {}

// PasswordResetCancelledEvent fires when [Person.CancelPasswordReset]
// invalidates a pending reset (admin action, security-incident
// response, or implicit cancel via direct password change while a
// reset was pending).
type PasswordResetCancelledEvent struct {
	PersonID ID
	Reason   string
	At       time.Time
}

func (PasswordResetCancelledEvent) isPersonEvent() {}

// GlobalSuspensionLiftedEvent fires when [Person.LiftGlobalSuspension]
// reverses a previous global suspension. Subscribers re-enable login
// + remove the SIEM block.
type GlobalSuspensionLiftedEvent struct {
	PersonID ID
	At       time.Time
}

func (GlobalSuspensionLiftedEvent) isPersonEvent() {}

// AccountLockedEvent fires when [Person.RegisterFailedLogin]
// crosses the [MaxFailedLogins] threshold and sets [Person.LockedUntil].
//
// Per NIST 800-63B §5.2.2 + OWASP Authentication Cheat Sheet 2025 §7.4
// brute-force defense — SIEM subscribers correlate the lockout with
// the calling IP / device label to spot enumeration sweeps. Auth
// subscribers do NOT need to revoke refresh-token families on lockout
// (no credential change); the lockout self-expires per LockoutDuration.
type AccountLockedEvent struct {
	PersonID    ID
	LockedUntil time.Time
	FailedCount int
	At          time.Time
}

func (AccountLockedEvent) isPersonEvent() {}

// AccountUnlockedEvent fires when [Person.RegisterSuccessfulLogin]
// runs against a previously-locked OR positive-failure-counter account.
// Marks the recovery; audit + SIEM subscribers use this to close out
// the lockout correlation window.
type AccountUnlockedEvent struct {
	PersonID ID
	At       time.Time
}

func (AccountUnlockedEvent) isPersonEvent() {}
