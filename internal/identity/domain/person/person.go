// Package person defines the Person aggregate — global identity in
// LeadKart's Auth0/Entra-ID-style identity model (per LeadKart .NET
// `multi-tenancy.md` "Identity model" doctrine).
//
// Person is NOT tenant-scoped. One Person can hold many TenantMemberships
// across tenants over time, but at any moment only ONE Membership is
// Active (DB-enforced via partial unique index per `database.md`).
//
// Aggregate composition:
//   - [ID]: UUIDv7 primary key.
//   - [email.Address]: globally unique system-wide.
//   - First/last name: profile data; stored on Person not Membership.
//   - [PasswordHash] + [SecurityStamp]: credential VOs (defined in
//     credential.go in this package).
//   - IsActive: rare global suspension (compliance/fraud/operator action).
//   - IsAnonymised: DPDP/GDPR right-to-erasure terminal state.
//
// Per-tenant context (Status, RoleAssignments, Designation, ReportsTo,
// JoinedAt) lives on [TenantMembership] in the sibling package.
package person

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
)

// ID is the Person primary key. UUIDv7 string for B-tree locality.
type ID string

// IsZero reports whether the ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// nameMaxLen caps first/last name fields. UI + audit pressure choose 100
// (Indian names with patronyms can run long; 100 chars is generous).
const nameMaxLen = 100

// Account-lockout thresholds per NIST 800-63B §5.2.2 + OWASP
// Authentication Cheat Sheet 2025 §7.4. Per-account counter (NOT
// per-IP — IP rate-limit is a separate concern at the HTTP edge).
//
//   - MaxFailedLogins: how many consecutive failed attempts in the
//     sliding window trigger a lockout.
//   - LockoutWindow:   how far back we look when deciding "consecutive"
//     — last_failed_login_at older than this resets the counter.
//   - LockoutDuration: how long [Person.LockedUntil] is set into the
//     future once the threshold is crossed.
//
// 10/15min/15min are Auth0 + Okta defaults; the values are deliberately
// exported so an operator-tunable PasswordPolicy override can read them
// as upper bounds in a future iteration.
const (
	MaxFailedLogins = 10
	LockoutWindow   = 15 * time.Minute
	LockoutDuration = 15 * time.Minute
)

// Person is the aggregate root.
type Person struct {
	id                     ID
	email                  email.Address
	firstName              string
	lastName               string
	passwordHash           PasswordHash
	securityStamp          SecurityStamp
	isActive               bool
	isAnonymised           bool
	isGloballySuspended    bool                 // rare global ban — compliance / fraud / cross-tenant abuse
	globalSuspensionReason string               // populated when isGloballySuspended; cleared on Lift
	globallySuspendedAt    time.Time            // zero unless suspended; reset on Lift
	pendingPasswordReset   PendingPasswordReset // zero unless reset requested + pending
	pendingEmailChange     PendingEmailChange   // zero unless email change requested + pending
	mustChangePassword     bool                 // BRD line 241 — true when admin/operator provisioned the credential
	failedLoginCount       int                  // NIST 800-63B §5.2.2 sliding-window counter
	lockedUntil            time.Time            // zero unless locked + LockedUntil > now
	lastFailedLoginAt      time.Time            // drives the sliding-window reset
	createdAt              time.Time
	anonymisedAt           time.Time
	events                 []Event
}

// New constructs a brand-new Person. Returns [ErrInvalid] (wrapped) on
// invariant violation. The aggregate emits [CreatedEvent] which the
// repository drains via [PullEvents] when persisting.
//
// `now` is the explicit instant for createdAt + event timestamp per the
// clock-injection refactor.
func New(id ID, e email.Address, firstName, lastName string, passwordHash PasswordHash, now time.Time) (*Person, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if e.IsZero() {
		return nil, fmt.Errorf("%w: email required", ErrInvalid)
	}
	if strings.TrimSpace(firstName) == "" {
		return nil, fmt.Errorf("%w: first name required", ErrInvalid)
	}
	if strings.TrimSpace(lastName) == "" {
		return nil, fmt.Errorf("%w: last name required", ErrInvalid)
	}
	if len(firstName) > nameMaxLen {
		return nil, fmt.Errorf("%w: first name too long (max %d)", ErrInvalid, nameMaxLen)
	}
	if len(lastName) > nameMaxLen {
		return nil, fmt.Errorf("%w: last name too long (max %d)", ErrInvalid, nameMaxLen)
	}
	if passwordHash.IsZero() {
		return nil, fmt.Errorf("%w: password hash required", ErrInvalid)
	}

	now = now.UTC()
	p := &Person{
		id:            id,
		email:         e,
		firstName:     firstName,
		lastName:      lastName,
		passwordHash:  passwordHash,
		securityStamp: NewSecurityStamp(),
		isActive:      true,
		isAnonymised:  false,
		createdAt:     now,
	}
	p.recordEvent(CreatedEvent{
		PersonID:  id,
		Email:     e,
		FirstName: firstName,
		LastName:  lastName,
		At:        now,
	})
	return p, nil
}

// NewWithMustChangePassword constructs a Person whose credential was
// provisioned by an admin/operator — per BRD line 241 the new Person
// MUST be forced through the change-password flow on first login.
//
// Used by RegisterTenant (the admin Person created during tenant
// onboarding) and CreateUser (a user Person whose initial password
// was chosen by the inviting admin). Self-rotated paths
// (ChangePassword + ConfirmPasswordReset) clear the flag.
//
// Wraps [New] so all invariant validation flows through one path —
// the only difference is the trailing flag.
func NewWithMustChangePassword(id ID, e email.Address, firstName, lastName string, passwordHash PasswordHash, now time.Time) (*Person, error) {
	p, err := New(id, e, firstName, lastName, passwordHash, now)
	if err != nil {
		return nil, err
	}
	p.mustChangePassword = true
	return p, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                          ID
	Email                       email.Address
	FirstName                   string
	LastName                    string
	PasswordHash                PasswordHash
	SecurityStamp               SecurityStamp
	IsActive                    bool
	IsAnonymised                bool
	IsGloballySuspended         bool
	GlobalSuspensionReason      string
	GloballySuspendedAt         time.Time
	PasswordResetTokenHash      PasswordResetTokenHash
	PasswordResetExpiresAt      time.Time
	PendingEmailChangeNewEmail  email.Address
	PendingEmailChangeHash      EmailChangeTokenHash
	PendingEmailChangeExpiresAt time.Time
	MustChangePassword          bool
	FailedLoginCount            int
	LockedUntil                 time.Time
	LastFailedLoginAt           time.Time
	CreatedAt                   time.Time
	AnonymisedAt                time.Time
}

// UnmarshalFromDB re-hydrates a Person from persistence. Used ONLY by
// the repository on read paths — does NOT re-validate (TDL canon).
func UnmarshalFromDB(s Snapshot) *Person {
	p := &Person{
		id:                     s.ID,
		email:                  s.Email,
		firstName:              s.FirstName,
		lastName:               s.LastName,
		passwordHash:           s.PasswordHash,
		securityStamp:          s.SecurityStamp,
		isActive:               s.IsActive,
		isAnonymised:           s.IsAnonymised,
		isGloballySuspended:    s.IsGloballySuspended,
		globalSuspensionReason: s.GlobalSuspensionReason,
		globallySuspendedAt:    s.GloballySuspendedAt,
		mustChangePassword:     s.MustChangePassword,
		failedLoginCount:       s.FailedLoginCount,
		lockedUntil:            s.LockedUntil,
		lastFailedLoginAt:      s.LastFailedLoginAt,
		createdAt:              s.CreatedAt,
		anonymisedAt:           s.AnonymisedAt,
	}
	if !s.PasswordResetTokenHash.IsZero() {
		p.pendingPasswordReset = PendingPasswordReset{
			hash:      s.PasswordResetTokenHash,
			expiresAt: s.PasswordResetExpiresAt,
		}
	}
	if !s.PendingEmailChangeHash.IsZero() {
		p.pendingEmailChange = PendingEmailChange{
			newEmail:  s.PendingEmailChangeNewEmail,
			hash:      s.PendingEmailChangeHash,
			expiresAt: s.PendingEmailChangeExpiresAt,
		}
	}
	return p
}

// ----- Getters --------------------------------------------------------------

// ID returns the Person primary key.
func (p *Person) ID() ID { return p.id }

// Email returns the globally-unique email.
func (p *Person) Email() email.Address { return p.email }

// FirstName returns the given name.
func (p *Person) FirstName() string { return p.firstName }

// LastName returns the family name.
func (p *Person) LastName() string { return p.lastName }

// PasswordHash returns the Argon2id PHC string. Caller must NOT log this.
func (p *Person) PasswordHash() PasswordHash { return p.passwordHash }

// SecurityStamp returns the current per-Person stamp. JWT issuance embeds
// this; subsequent requests validate against the stored stamp.
func (p *Person) SecurityStamp() SecurityStamp { return p.securityStamp }

// IsActive reports whether the Person can authenticate. False after global
// suspension or anonymisation.
func (p *Person) IsActive() bool { return p.isActive }

// IsAnonymised reports whether DPDP/GDPR right-to-erasure has been applied.
// Anonymised Persons are terminal — never re-activate.
func (p *Person) IsAnonymised() bool { return p.isAnonymised }

// CreatedAt returns the registration timestamp.
func (p *Person) CreatedAt() time.Time { return p.createdAt }

// AnonymisedAt returns the anonymisation timestamp; zero if not anonymised.
func (p *Person) AnonymisedAt() time.Time { return p.anonymisedAt }

// IsGloballySuspended reports whether the Person is globally banned
// (rare — compliance, fraud, cross-tenant abuse). Distinct from
// [Person.IsActive] (deactivation) and Membership.Status (per-tenant).
func (p *Person) IsGloballySuspended() bool { return p.isGloballySuspended }

// GlobalSuspensionReason returns the audit reason supplied when
// GloballySuspend was called. Empty if not globally suspended.
func (p *Person) GlobalSuspensionReason() string { return p.globalSuspensionReason }

// GloballySuspendedAt returns the timestamp of the most recent global
// suspension. Zero if not currently suspended.
func (p *Person) GloballySuspendedAt() time.Time { return p.globallySuspendedAt }

// PendingPasswordReset returns the per-Person reset sub-state. Zero
// value means no reset is pending. Repository adapter reads
// `.Hash().IsZero()` to decide whether to write the reset columns.
func (p *Person) PendingPasswordReset() PendingPasswordReset { return p.pendingPasswordReset }

// PendingEmailChange returns the per-Person email-change sub-state.
// Zero value means no change is pending.
func (p *Person) PendingEmailChange() PendingEmailChange { return p.pendingEmailChange }

// MustChangePassword reports whether the credential was provisioned by
// an admin/operator (BRD line 241). True forces the change-password
// flow on first login; cleared on self-changed/self-reset paths.
func (p *Person) MustChangePassword() bool { return p.mustChangePassword }

// FailedLoginCount returns the current sliding-window failure count.
// Reset to 0 on successful login OR when [LockoutWindow] elapses since
// [Person.LastFailedLoginAt].
func (p *Person) FailedLoginCount() int { return p.failedLoginCount }

// LockedUntil returns the lockout-expiry timestamp; zero when never
// locked or after a successful login cleared it. The aggregate's
// [Person.IsLocked] is the canonical predicate — callers should not
// compare against `now` themselves.
func (p *Person) LockedUntil() time.Time { return p.lockedUntil }

// LastFailedLoginAt returns the timestamp of the most recent failed
// login attempt; zero when [Person.FailedLoginCount] is 0.
func (p *Person) LastFailedLoginAt() time.Time { return p.lastFailedLoginAt }

// IsLocked reports whether the account is currently locked as of `now`.
// Per security.md "Login flow ordering": login MUST consult this BEFORE
// bcrypt-verifying the supplied password, otherwise an attacker can
// distinguish locked-vs-unlocked accounts by timing.
func (p *Person) IsLocked(now time.Time) bool {
	if p.lockedUntil.IsZero() {
		return false
	}
	return now.Before(p.lockedUntil)
}

// ----- State transitions ----------------------------------------------------

// ChangePassword updates the password hash AND rotates the SecurityStamp.
//
// Stamp rotation invalidates all outstanding JWTs on next request — that's
// the security guarantee per `security.md` "SecurityStamp rotation triggers".
//
// Side-effect: clears [Person.MustChangePassword]. A self-initiated
// password change satisfies the BRD-line-241 forced-rotation gate.
//
// Idempotency: NOT idempotent. Each call rotates the stamp + emits an event.
func (p *Person) ChangePassword(newHash PasswordHash, now time.Time) error {
	if newHash.IsZero() {
		return fmt.Errorf("%w: new password hash required", ErrInvalid)
	}
	now = now.UTC()
	p.passwordHash = newHash
	p.securityStamp = NewSecurityStamp()
	p.mustChangePassword = false
	p.recordEvent(PasswordChangedEvent{
		PersonID: p.id,
		At:       now,
	})
	return nil
}

// UpdateProfile changes the Person's display name (FirstName + LastName).
//
// Validation mirrors [New]: both fields required, trimmed-non-empty, capped
// at nameMaxLen. Refused on anonymised Persons (DPDP Art. 17 cascade
// already replaced the names with "anonymised"; re-personalising would
// undo the erasure). No-op when both fields equal current values
// (idempotent — second call with same values emits no event).
//
// Emits [ProfileUpdatedEvent] with OLD + NEW values for audit /
// integration-event subscribers.
//
// Does NOT rotate SecurityStamp — profile changes are not a security
// boundary per `security.md` "SecurityStamp rotation triggers" (only
// password / role / permission / email / logout-all rotate).
func (p *Person) UpdateProfile(firstName, lastName string, now time.Time) error {
	if p.isAnonymised {
		return fmt.Errorf("%w: cannot update profile of anonymised person", ErrInvalid)
	}
	if strings.TrimSpace(firstName) == "" {
		return fmt.Errorf("%w: first name required", ErrInvalid)
	}
	if strings.TrimSpace(lastName) == "" {
		return fmt.Errorf("%w: last name required", ErrInvalid)
	}
	if len(firstName) > nameMaxLen {
		return fmt.Errorf("%w: first name too long (max %d)", ErrInvalid, nameMaxLen)
	}
	if len(lastName) > nameMaxLen {
		return fmt.Errorf("%w: last name too long (max %d)", ErrInvalid, nameMaxLen)
	}
	if firstName == p.firstName && lastName == p.lastName {
		return nil
	}
	old := struct{ first, last string }{p.firstName, p.lastName}
	p.firstName = firstName
	p.lastName = lastName
	p.recordEvent(ProfileUpdatedEvent{
		PersonID:     p.id,
		OldFirstName: old.first,
		OldLastName:  old.last,
		NewFirstName: firstName,
		NewLastName:  lastName,
		At:           now.UTC(),
	})
	return nil
}

// RequestPasswordReset opens a one-shot password-reset window. The
// caller has already minted a high-entropy raw token, hashed it via
// SHA-256, and supplies the hash + an absolute TTL.
//
// Per Auth0 / Okta / Microsoft Entra ID canon: at-most-one pending
// reset — the most recent request supersedes any prior. The previous
// emailed token is silently invalidated. This avoids ambiguity when a
// user clicks "forgot password" twice; without this rule both old +
// new tokens would validate, expanding the attack window.
//
// ttl MUST be positive; spec is caller's choice but security.md
// recommends 1h for high-traffic SaaS, up to 24h for low-traffic B2B.
//
// Rejected on:
//   - Anonymised Person (cannot reset; the credential is gone)
//   - Globally-suspended Person (no path back via self-service reset
//     during suspension; admin must lift first)
//
// Allowed on inactive Persons (admin may need to reset before
// reactivation per Identity workflow).
//
// Does NOT rotate SecurityStamp — the reset hasn't happened yet,
// only the request. SecurityStamp rotates inside ConfirmPasswordReset.
//
// Emits TWO events per ADR 0057:
//
//   - [PasswordResetRequestedEvent]  — AUDIT signal (no plaintext).
//   - [PasswordResetEmailRequestedEvent] — ACTION signal carrying the
//     plaintext token + recipient details for the async email
//     subscriber. The plaintext NEVER hits the row state; it lives in
//     the recordEvent transient buffer + the outbox payload exactly
//     once.
//
// plaintextToken MUST be the same plaintext the caller hashed into
// tokenHash — the aggregate cannot verify this invariant (one-way
// hash); the application service is the boundary that guarantees
// consistency. Empty plaintextToken means "no email needed" (admin
// hotwire path); the AUDIT event still fires.
func (p *Person) RequestPasswordReset(plaintextToken string, tokenHash PasswordResetTokenHash, ttl time.Duration, now time.Time) error {
	if tokenHash.IsZero() {
		return fmt.Errorf("%w: reset token hash required", ErrInvalid)
	}
	if ttl <= 0 {
		return fmt.Errorf("%w: reset ttl must be positive (got %v)", ErrInvalid, ttl)
	}
	if p.isAnonymised {
		return fmt.Errorf("%w: cannot reset password for anonymised person", ErrInvalid)
	}
	if p.isGloballySuspended {
		return fmt.Errorf("%w: cannot reset password while globally suspended", ErrInvalid)
	}
	now = now.UTC()
	expiresAt := now.Add(ttl)
	p.pendingPasswordReset = PendingPasswordReset{
		hash:      tokenHash,
		expiresAt: expiresAt,
	}
	p.recordEvent(PasswordResetRequestedEvent{
		PersonID:  p.id,
		ExpiresAt: expiresAt,
		At:        now,
	})
	if plaintextToken != "" {
		p.recordEvent(PasswordResetEmailRequestedEvent{
			PersonID:       p.id,
			Email:          p.email,
			PlaintextToken: plaintextToken,
			ExpiresAt:      expiresAt,
			RecipientName:  p.firstName,
			At:             now,
		})
	}
	return nil
}

// ConfirmPasswordReset applies a new password by presenting a valid
// reset token. Caller hashes the user-presented raw token (SHA-256)
// and supplies the hash; aggregate constant-time-compares against
// the stored hash.
//
// Effects on success:
//   - passwordHash set to newHash.
//   - SecurityStamp rotated (kills outstanding JWTs).
//   - pendingPasswordReset cleared (one-shot — token consumed).
//   - Both PasswordResetConfirmedEvent + PasswordChangedEvent emitted
//     (the narrower Confirmed event lets compliance subscribers
//     distinguish reset-flow from any-other password-change flow;
//     the broader Changed event is the load-bearing security signal
//     downstream auth subscribers consume to revoke families).
//
// Rejected on:
//   - No pending reset — caller has nothing to confirm.
//   - Token hash mismatch — defeats brute-force; same error shape
//     as expired so attackers can't distinguish "wrong token" from
//     "expired token" via timing.
//   - Expired (now > expiresAt at call site).
//   - Anonymised / globally-suspended Person — same as Request gates.
//   - Zero newHash.
//
// Idempotency: NOT idempotent. Each successful call rotates the
// SecurityStamp + emits events. The pending reset is single-use.
func (p *Person) ConfirmPasswordReset(presentedHash PasswordResetTokenHash, newHash PasswordHash, now time.Time) error {
	if newHash.IsZero() {
		return fmt.Errorf("%w: new password hash required", ErrInvalid)
	}
	if presentedHash.IsZero() {
		return fmt.Errorf("%w: presented reset token hash required", ErrInvalid)
	}
	if p.isAnonymised {
		return fmt.Errorf("%w: cannot reset password for anonymised person", ErrInvalid)
	}
	if p.isGloballySuspended {
		return fmt.Errorf("%w: cannot reset password while globally suspended", ErrInvalid)
	}
	if p.pendingPasswordReset.IsZero() {
		return fmt.Errorf("%w: no pending password reset", ErrInvalid)
	}
	now = now.UTC()
	if !now.Before(p.pendingPasswordReset.expiresAt) {
		// Clear the expired token defensively so subsequent calls don't
		// bother comparing — same resulting state, no event emitted.
		p.pendingPasswordReset = PendingPasswordReset{}
		return fmt.Errorf("%w: reset token expired", ErrInvalid)
	}
	if !p.pendingPasswordReset.hash.Equal(presentedHash) {
		return fmt.Errorf("%w: reset token mismatch", ErrInvalid)
	}
	// Success — apply, rotate, clear, emit. The forced-rotation gate
	// (BRD line 241) is satisfied: clearing must_change_password lets
	// the next login proceed without the change-password prompt.
	p.passwordHash = newHash
	p.securityStamp = NewSecurityStamp()
	p.pendingPasswordReset = PendingPasswordReset{}
	p.mustChangePassword = false
	p.recordEvent(PasswordResetConfirmedEvent{
		PersonID: p.id,
		At:       now,
	})
	p.recordEvent(PasswordChangedEvent{
		PersonID: p.id,
		At:       now,
	})
	return nil
}

// CancelPasswordReset invalidates a pending reset without applying a
// new password. Reasons (string supplied by caller, audit-required):
//   - operator action (admin cleared a stuck reset)
//   - security-incident response (token leaked, kill the window)
//   - implicit cancel via direct password change (the change-password
//     flow calls this before its own update to prevent the pending
//     emailed token from being usable post-change)
//
// Idempotent — no-op when no reset is pending.
//
// Does NOT rotate SecurityStamp — the cancel is purely state cleanup;
// no credential changed. (If the caller is canceling because the
// account is compromised, they should call ChangePassword which DOES
// rotate.)
func (p *Person) CancelPasswordReset(reason string, now time.Time) error {
	if p.pendingPasswordReset.IsZero() {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: cancel reason required for audit", ErrInvalid)
	}
	p.pendingPasswordReset = PendingPasswordReset{}
	p.recordEvent(PasswordResetCancelledEvent{
		PersonID: p.id,
		Reason:   reason,
		At:       now.UTC(),
	})
	return nil
}

// RequestEmailChange opens an email-change confirmation window. The
// caller has already minted a high-entropy raw token, hashed it via
// SHA-256, and supplies the proposed new email + hash + TTL.
//
// At-most-one-pending invariant per Person — a fresh request
// supersedes any prior pending change. Industry canon (Auth0, Stripe,
// Microsoft Entra ID): most-recent wins, prior emailed token silently
// invalidated.
//
// Validation:
//   - newEmail MUST be non-zero and DIFFERENT from current email
//     (no-op rejected — prevents accidental "change to same address"
//     from emitting a spurious confirmation request)
//   - tokenHash non-zero
//   - ttl positive
//
// Rejected on:
//   - Anonymised Person (terminal — email is the FK; cannot rebind)
//   - Globally-suspended Person (no path forward without operator lift)
//
// Does NOT rotate SecurityStamp — change hasn't applied yet, only
// the request. SecurityStamp rotates inside ConfirmEmailChange.
//
// CALLER INVARIANT: the application service MUST verify the proposed
// new email is not already taken globally before calling this method
// (per multi-tenancy.md "Email is globally unique system-wide" partial
// unique index). Domain trusts the boundary check; the Repository
// will fail the persist if a race materialises a duplicate.
//
// Emits TWO events per ADR 0057:
//
//   - [EmailChangeRequestedEvent]  — AUDIT signal (no plaintext).
//   - [EmailChangeConfirmationRequestedEvent] — ACTION signal carrying
//     the plaintext token + new + old addresses for the email
//     subscriber. Same two-event pattern as RequestPasswordReset.
func (p *Person) RequestEmailChange(newEmail email.Address, plaintextToken string, tokenHash EmailChangeTokenHash, ttl time.Duration, now time.Time) error {
	if newEmail.IsZero() {
		return fmt.Errorf("%w: new email required", ErrInvalid)
	}
	if newEmail.String() == p.email.String() {
		return fmt.Errorf("%w: new email same as current", ErrInvalid)
	}
	if tokenHash.IsZero() {
		return fmt.Errorf("%w: email change token hash required", ErrInvalid)
	}
	if ttl <= 0 {
		return fmt.Errorf("%w: email change ttl must be positive (got %v)", ErrInvalid, ttl)
	}
	if p.isAnonymised {
		return fmt.Errorf("%w: cannot change email of anonymised person", ErrInvalid)
	}
	if p.isGloballySuspended {
		return fmt.Errorf("%w: cannot change email while globally suspended", ErrInvalid)
	}
	now = now.UTC()
	expiresAt := now.Add(ttl)
	p.pendingEmailChange = PendingEmailChange{
		newEmail:  newEmail,
		hash:      tokenHash,
		expiresAt: expiresAt,
	}
	p.recordEvent(EmailChangeRequestedEvent{
		PersonID:  p.id,
		NewEmail:  newEmail,
		ExpiresAt: expiresAt,
		At:        now,
	})
	if plaintextToken != "" {
		p.recordEvent(EmailChangeConfirmationRequestedEvent{
			PersonID:       p.id,
			NewEmail:       newEmail,
			OldEmail:       p.email,
			PlaintextToken: plaintextToken,
			ExpiresAt:      expiresAt,
			RecipientName:  p.firstName,
			At:             now,
		})
	}
	return nil
}

// ConfirmEmailChange applies the pending new email by presenting a
// valid confirmation token. Caller hashes the user-presented raw
// token (SHA-256) and supplies the hash; aggregate constant-time-
// compares against the stored hash.
//
// Effects on success:
//   - email rotated to the pending new email.
//   - SecurityStamp rotated (kills outstanding JWTs — `sub` rebound).
//   - pendingEmailChange cleared (single-use).
//   - EmailChangedEvent emitted with OLD + NEW for audit.
//
// Rejected on:
//   - No pending change.
//   - Token hash mismatch (defensive: pending preserved for retry
//     within the window — same shape as ConfirmPasswordReset).
//   - Expired (defensive cleanup: pending cleared so subsequent calls
//     see "no pending" rather than re-rejecting on expiry).
//   - Anonymised / globally-suspended.
//
// NOT idempotent — single-use token; second call returns "no pending".
func (p *Person) ConfirmEmailChange(presentedHash EmailChangeTokenHash, now time.Time) error {
	if presentedHash.IsZero() {
		return fmt.Errorf("%w: presented email change token hash required", ErrInvalid)
	}
	if p.isAnonymised {
		return fmt.Errorf("%w: cannot change email of anonymised person", ErrInvalid)
	}
	if p.isGloballySuspended {
		return fmt.Errorf("%w: cannot change email while globally suspended", ErrInvalid)
	}
	if p.pendingEmailChange.IsZero() {
		return fmt.Errorf("%w: no pending email change", ErrInvalid)
	}
	now = now.UTC()
	if !now.Before(p.pendingEmailChange.expiresAt) {
		p.pendingEmailChange = PendingEmailChange{}
		return fmt.Errorf("%w: email change token expired", ErrInvalid)
	}
	if !p.pendingEmailChange.hash.Equal(presentedHash) {
		return fmt.Errorf("%w: email change token mismatch", ErrInvalid)
	}
	oldEmail := p.email
	p.email = p.pendingEmailChange.newEmail
	p.securityStamp = NewSecurityStamp()
	p.pendingEmailChange = PendingEmailChange{}
	p.recordEvent(EmailChangedEvent{
		PersonID: p.id,
		OldEmail: oldEmail,
		NewEmail: p.email,
		At:       now,
	})
	return nil
}

// CancelEmailChange invalidates a pending email change without
// applying it. Reasons: admin action, security incident response,
// implicit cancel via direct admin email update.
//
// Idempotent — no-op when no change is pending. Audit reason
// required when pending. Does NOT rotate SecurityStamp (no
// credential changed).
func (p *Person) CancelEmailChange(reason string, now time.Time) error {
	if p.pendingEmailChange.IsZero() {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: cancel reason required for audit", ErrInvalid)
	}
	p.pendingEmailChange = PendingEmailChange{}
	p.recordEvent(EmailChangeCancelledEvent{
		PersonID: p.id,
		Reason:   reason,
		At:       now.UTC(),
	})
	return nil
}

// GloballySuspend marks the Person as globally banned across every
// tenant. Rare action — compliance violation, fraud, cross-tenant
// abuse. Distinct from [Person.Anonymise] (irreversible PII scrub)
// and Membership.Deactivate (per-tenant; Person can still log into
// other tenants).
//
// Effects:
//   - isGloballySuspended → true; reason recorded for audit.
//   - SecurityStamp rotated (invalidates every outstanding JWT
//     immediately per security.md "SecurityStamp rotation triggers").
//   - Login flow MUST reject suspended Persons before password verify.
//
// reason MUST be non-empty (audit requirement). Idempotent only when
// reason matches existing suspension; rejected on conflicting reason
// (audit-trail integrity).
//
// Rejected if Person is anonymised (terminal — already scrubbed).
func (p *Person) GloballySuspend(reason string, now time.Time) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: global suspension reason required for audit", ErrInvalid)
	}
	if p.isAnonymised {
		return fmt.Errorf("%w: cannot suspend anonymised person", ErrInvalid)
	}
	if p.isGloballySuspended {
		if p.globalSuspensionReason == reason {
			return nil
		}
		return fmt.Errorf("%w: person already suspended (reason: %q)", ErrInvalid, p.globalSuspensionReason)
	}
	now = now.UTC()
	p.isGloballySuspended = true
	p.globalSuspensionReason = reason
	p.globallySuspendedAt = now
	p.securityStamp = NewSecurityStamp()
	p.recordEvent(GloballySuspendedEvent{
		PersonID: p.id,
		Reason:   reason,
		At:       now,
	})
	return nil
}

// LiftGlobalSuspension reverses a previous global suspension.
// Operator action; SecurityStamp NOT rotated again on lift (rotation
// already happened at suspension time; lifting just clears the flag).
//
// Idempotent: no-op when not currently suspended.
func (p *Person) LiftGlobalSuspension(now time.Time) error {
	if !p.isGloballySuspended {
		return nil
	}
	p.isGloballySuspended = false
	p.globalSuspensionReason = ""
	p.globallySuspendedAt = time.Time{}
	p.recordEvent(GlobalSuspensionLiftedEvent{
		PersonID: p.id,
		At:       now.UTC(),
	})
	return nil
}

// Anonymise scrubs PII per DPDP Act 2023 §12 / GDPR Art. 17 right-to-erasure.
//
// Effects:
//   - First + last names replaced with literal "anonymised".
//   - Email kept (FK integrity); upstream caller may have already replaced.
//   - IsActive flipped to false.
//   - IsAnonymised set to true.
//   - SecurityStamp rotated (invalidates outstanding JWTs).
//   - AnonymisedAt timestamp recorded.
//
// Idempotent — second call on already-anonymised Person is a no-op.
//
// NOTE: This is the aggregate-level scrub. A separate orchestrator
// fans out [AnonymisedEvent] to other modules so they scrub their own
// PII (CRM lead notes, Tasks comments, etc.).
func (p *Person) Anonymise(now time.Time) error {
	if p.isAnonymised {
		return nil
	}
	now = now.UTC()
	p.firstName = "anonymised"
	p.lastName = "anonymised"
	p.isActive = false
	p.isAnonymised = true
	p.anonymisedAt = now
	p.securityStamp = NewSecurityStamp()
	p.recordEvent(AnonymisedEvent{
		PersonID: p.id,
		At:       now,
	})
	return nil
}

// MarkPasswordChanged clears [Person.MustChangePassword] without
// rotating the credential or the SecurityStamp.
//
// Used by the login flow when an operator-provisioned credential was
// verified successfully + the v0.3 strict-middleware enforcement
// (forced redirect to change-password) lands. v0.2 doesn't call this —
// the flag is cleared via the change-password / confirm-reset paths
// instead. Kept on the aggregate so the future middleware lands
// without re-opening domain.
//
// No-op when already false.
func (p *Person) MarkPasswordChanged() {
	if !p.mustChangePassword {
		return
	}
	p.mustChangePassword = false
}

// RegisterFailedLogin records a failed login attempt + applies the
// NIST 800-63B §5.2.2 lockout rule. Caller supplies `now` so the
// sliding-window arithmetic stays deterministic + testable (no
// clock.Now() inside the aggregate for hot-path counters).
//
// Sliding-window semantics:
//   - If `now` is more than [LockoutWindow] after the last failure,
//     the counter is RESET to 1 (the current attempt) rather than
//     incremented — older failures don't accumulate.
//   - Otherwise, increment.
//   - If the new count reaches [MaxFailedLogins], set
//     [Person.LockedUntil] to now+LockoutDuration + emit
//     [PersonAccountLockedEvent].
//
// Does NOT rotate SecurityStamp — a failed login is not a credential
// change. The outbox event lets SIEM subscribers correlate the
// lockout with the IP / device.
//
// Idempotency: NOT idempotent. Each call advances state.
func (p *Person) RegisterFailedLogin(now time.Time) {
	// Sliding-window reset — if the previous failure is outside the
	// window, the current attempt is "the first" of a fresh window.
	if !p.lastFailedLoginAt.IsZero() && now.Sub(p.lastFailedLoginAt) > LockoutWindow {
		p.failedLoginCount = 0
	}
	p.failedLoginCount++
	p.lastFailedLoginAt = now
	if p.failedLoginCount >= MaxFailedLogins && p.lockedUntil.IsZero() {
		p.lockedUntil = now.Add(LockoutDuration)
		p.recordEvent(AccountLockedEvent{
			PersonID:    p.id,
			LockedUntil: p.lockedUntil,
			FailedCount: p.failedLoginCount,
			At:          now,
		})
	}
}

// RegisterSuccessfulLogin clears the failed-login counter + the
// lockout-expiry. If the account was previously locked OR carried a
// positive failure count, emits [PersonAccountUnlockedEvent] so SIEM
// subscribers + audit can correlate the recovery.
//
// Does NOT rotate SecurityStamp — a successful login doesn't change
// the credential, only refreshes its statistical state.
//
// Idempotency: callable any number of times; no-op when the counter
// + lockout state is already clear.
func (p *Person) RegisterSuccessfulLogin(now time.Time) {
	hadFailures := p.failedLoginCount > 0 || !p.lockedUntil.IsZero()
	p.failedLoginCount = 0
	p.lockedUntil = time.Time{}
	p.lastFailedLoginAt = time.Time{}
	if hadFailures {
		p.recordEvent(AccountUnlockedEvent{
			PersonID: p.id,
			At:       now.UTC(),
		})
	}
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains recorded events. See tenant.PullEvents for semantics.
func (p *Person) PullEvents() []Event {
	if len(p.events) == 0 {
		return nil
	}
	out := p.events
	p.events = nil
	return out
}

func (p *Person) recordEvent(e Event) {
	p.events = append(p.events, e)
}
