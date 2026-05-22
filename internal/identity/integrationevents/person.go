package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// PersonCreatedV1 — a new global Person identity has been added (first
// time this email enters the system). Consumed by Notifications
// (welcome email) + future modules.
//
// Platform-scoped: Person is global identity; no per-tenant context
// per `multi-tenancy.md` "Identity model".
type PersonCreatedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	Email         string    `json:"email"`
	FirstName     string    `json:"first_name"`
	LastName      string    `json:"last_name"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonCreatedV1) Topic() string { return "identity.person_created.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonPasswordChangedV1 — Person rotated their password. The
// SecurityStamp on the Person changed; Identity subscribers MUST
// revoke every refresh-token family for this Person across tenants
// (logout-all-sessions choreography per `security.md` "SecurityStamp
// rotation triggers").
type PersonPasswordChangedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonPasswordChangedV1) Topic() string { return "identity.person_password_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonPasswordChangedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonProfileUpdatedV1 — Person changed their display name
// (FirstName + LastName). Platform-scoped per the .NET parent's
// vocabulary split (Person fields global; Membership profile fields
// tenant-scoped — see MembershipProfileUpdatedV1 for the tenant-scoped
// counterpart). Consumed by Notifications (display-name update) +
// audit.
type PersonProfileUpdatedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OldFirstName  string    `json:"old_first_name"`
	OldLastName   string    `json:"old_last_name"`
	NewFirstName  string    `json:"new_first_name"`
	NewLastName   string    `json:"new_last_name"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonProfileUpdatedV1) Topic() string { return "identity.person_profile_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonProfileUpdatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonGloballySuspendedV1 — Person was globally banned (compliance,
// fraud, cross-tenant abuse). Distinct from PersonAnonymisedV1
// (irreversible PII scrub) and Membership.Deactivated (per-tenant).
//
// Auth subscribers MUST kill every refresh-token family for this
// PersonID across tenants AND block login attempts. Notifications +
// SIEM subscribers may surface alerts.
type PersonGloballySuspendedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonGloballySuspendedV1) Topic() string { return "identity.person_globally_suspended.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonGloballySuspendedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonGlobalSuspensionLiftedV1 — global suspension reversed by
// operator. Subscribers re-enable login + remove SIEM block.
type PersonGlobalSuspensionLiftedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonGlobalSuspensionLiftedV1) Topic() string {
	return "identity.person_global_suspension_lifted.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonGlobalSuspensionLiftedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonAnonymisedV1 — DPDP Act §12 / GDPR Art. 17 right-to-erasure
// completed at the Person aggregate. Cascades to every module touching
// the Person's PII per `data-retention.md` (CRM lead notes scrub,
// Tasks comment scrub, etc.) + Identity revokes every refresh-token
// family across tenants.
type PersonAnonymisedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonAnonymisedV1) Topic() string { return "identity.person_anonymised.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonAnonymisedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonPasswordResetRequestedV1 — Person initiated a forgot-password
// flow. The raw plaintext token is delivered out-of-band by the
// command handler's email gateway BEFORE this event fires; the event
// is the audit/observability marker that the request happened. The
// hash-only column on the Person row is the security-critical state.
//
// Email lookup deferred to subscribers via Person.GetByID since the
// domain event itself doesn't carry the email — keeps the event
// payload small + avoids stale data if the Person email changed
// between request + delivery.
type PersonPasswordResetRequestedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	ExpiresAtUTC  time.Time `json:"expires_at_utc"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonPasswordResetRequestedV1) Topic() string {
	return "identity.person_password_reset_requested.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonPasswordResetRequestedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonPasswordResetConfirmedV1 — reset token accepted; password
// rotated. PersonPasswordChangedV1 ALSO fires (the aggregate emits
// both — narrower flow-marker + broader security-critical signal).
// The cascade subscribers (revoke families) react to PasswordChanged;
// audit / compliance subscribers may distinguish the reset path via
// this event.
type PersonPasswordResetConfirmedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonPasswordResetConfirmedV1) Topic() string {
	return "identity.person_password_reset_confirmed.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonPasswordResetConfirmedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonPasswordResetCancelledV1 — pending reset cleared without
// confirmation (user cancelled, password changed directly, etc.).
// Audit-only; no cascade.
type PersonPasswordResetCancelledV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonPasswordResetCancelledV1) Topic() string {
	return "identity.person_password_reset_cancelled.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonPasswordResetCancelledV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonEmailChangeRequestedV1 — Person initiated email change.
// Notifications subscriber emails the confirmation link to the NEW
// address. Auth0/Okta canon: confirmation goes to NEW (proves
// control), informational to OLD post-confirmation. Old-email field
// is omitted (the domain event doesn't carry it; subscribers that
// want OLD lookup via Person.GetByID before the change confirms).
type PersonEmailChangeRequestedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	NewEmail      string    `json:"new_email"`
	ExpiresAtUTC  time.Time `json:"expires_at_utc"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonEmailChangeRequestedV1) Topic() string {
	return "identity.person_email_change_requested.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonEmailChangeRequestedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonEmailChangedV1 — confirmation token accepted; Person email
// rotated. Email is the global identity primary; changing it
// invalidates every authenticated session per `security.md`
// SecurityStamp rotation triggers. Identity subscribers MUST revoke
// every refresh-token family for this Person across tenants. The
// SecurityStamp on the Person aggregate is also rotated by the
// aggregate method.
type PersonEmailChangedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OldEmail      string    `json:"old_email"`
	NewEmail      string    `json:"new_email"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonEmailChangedV1) Topic() string { return "identity.person_email_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonEmailChangedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonEmailChangeCancelledV1 — pending email change cleared without
// confirmation. Audit-only; no cascade.
type PersonEmailChangeCancelledV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonEmailChangeCancelledV1) Topic() string {
	return "identity.person_email_change_cancelled.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonEmailChangeCancelledV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonPasswordResetEmailRequestedV1 is the ACTION-side counterpart
// to [PersonPasswordResetRequestedV1] per ADR 0057 — carries the
// plaintext reset token + recipient address so a Watermill subscriber
// (the email sender) can deliver the link asynchronously.
//
// Why a separate event with plaintext:
//   - PersonPasswordResetRequestedV1   = audit/SIEM signal (no plaintext).
//   - PersonPasswordResetEmailRequestedV1 = single-consumer action signal.
//
// Security boundary on the plaintext: outbox is RLS+FORCE + tenant-
// isolated; forwarder drains within ~1s; token TTL is 1h. The
// at-rest window for plaintext is ≤1s typical, ≤1h bound. Same
// boundary Stripe / Auth0 use for short-lived OTP-style delivery via
// async queues (per ADR 0057 §"Plaintext-in-outbox security analysis").
//
// Platform-scoped: the operation is global identity (no per-tenant
// context).
type PersonPasswordResetEmailRequestedV1 struct {
	platformMarker

	PersonID       uuid.UUID `json:"person_id"`
	Email          string    `json:"email"`
	PlaintextToken string    `json:"plaintext_token"`
	ExpiresAtUTC   time.Time `json:"expires_at_utc"`
	RecipientName  string    `json:"recipient_name"`
	OccurredAtUTC  time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonPasswordResetEmailRequestedV1) Topic() string {
	return "identity.person_password_reset_email_requested.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonPasswordResetEmailRequestedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonEmailChangeConfirmationRequestedV1 is the ACTION-side
// counterpart to [PersonEmailChangeRequestedV1] per ADR 0057 — carries
// the plaintext confirmation token + new+old addresses so the email
// subscriber can deliver the confirmation link asynchronously to the
// NEW address (Auth0/Okta canon).
//
// OldEmail is carried for a future "inform the OLD address" subscriber
// (deferred per ADR 0057 §"Deferred work"); current shape is plaintext-
// only to the NEW address.
type PersonEmailChangeConfirmationRequestedV1 struct {
	platformMarker

	PersonID       uuid.UUID `json:"person_id"`
	NewEmail       string    `json:"new_email"`
	OldEmail       string    `json:"old_email"`
	PlaintextToken string    `json:"plaintext_token"`
	ExpiresAtUTC   time.Time `json:"expires_at_utc"`
	RecipientName  string    `json:"recipient_name"`
	OccurredAtUTC  time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonEmailChangeConfirmationRequestedV1) Topic() string {
	return "identity.person_email_change_confirmation_requested.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonEmailChangeConfirmationRequestedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonAccountLockedV1 — Person hit the [person.MaxFailedLogins]
// threshold; account is now locked until LockedUntilUTC. Platform-
// scoped (account is global; lockout doesn't carry a tenant).
//
// Per NIST 800-63B §5.2.2 + OWASP Authentication Cheat Sheet 2025
// §7.4 brute-force defense — SIEM subscribers correlate with the
// calling IP / device label to spot enumeration sweeps. Notifications
// MAY send "your account was locked" email (Auth0 / Okta default; the
// .NET parent enables this by default).
type PersonAccountLockedV1 struct {
	platformMarker

	PersonID        uuid.UUID `json:"person_id"`
	LockedUntilUTC  time.Time `json:"locked_until_utc"`
	FailedCount     int       `json:"failed_count"`
	OccurredAtUTC   time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonAccountLockedV1) Topic() string { return "identity.person_account_locked.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonAccountLockedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonAccountUnlockedV1 — Person logged in successfully after one
// or more failed attempts (or after a prior lockout). Closes out the
// SIEM correlation window opened by [PersonAccountLockedV1].
// Platform-scoped (account is global).
type PersonAccountUnlockedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonAccountUnlockedV1) Topic() string { return "identity.person_account_unlocked.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonAccountUnlockedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// Compile-time assertions + registration.
var (
	_ Platform = PersonCreatedV1{}
	_ Platform = PersonPasswordChangedV1{}
	_ Platform = PersonProfileUpdatedV1{}
	_ Platform = PersonGloballySuspendedV1{}
	_ Platform = PersonGlobalSuspensionLiftedV1{}
	_ Platform = PersonAnonymisedV1{}
	_ Platform = PersonPasswordResetRequestedV1{}
	_ Platform = PersonPasswordResetConfirmedV1{}
	_ Platform = PersonPasswordResetCancelledV1{}
	_ Platform = PersonPasswordResetEmailRequestedV1{}
	_ Platform = PersonEmailChangeRequestedV1{}
	_ Platform = PersonEmailChangedV1{}
	_ Platform = PersonEmailChangeCancelledV1{}
	_ Platform = PersonEmailChangeConfirmationRequestedV1{}
	_ Platform = PersonAccountLockedV1{}
	_ Platform = PersonAccountUnlockedV1{}

	_ = register(PersonCreatedV1{})
	_ = register(PersonPasswordChangedV1{})
	_ = register(PersonProfileUpdatedV1{})
	_ = register(PersonGloballySuspendedV1{})
	_ = register(PersonGlobalSuspensionLiftedV1{})
	_ = register(PersonAnonymisedV1{})
	_ = register(PersonPasswordResetRequestedV1{})
	_ = register(PersonPasswordResetConfirmedV1{})
	_ = register(PersonPasswordResetCancelledV1{})
	_ = register(PersonPasswordResetEmailRequestedV1{})
	_ = register(PersonEmailChangeRequestedV1{})
	_ = register(PersonEmailChangedV1{})
	_ = register(PersonEmailChangeCancelledV1{})
	_ = register(PersonEmailChangeConfirmationRequestedV1{})
	_ = register(PersonAccountLockedV1{})
	_ = register(PersonAccountUnlockedV1{})
)
