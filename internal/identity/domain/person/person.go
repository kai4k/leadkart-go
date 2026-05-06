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

	"github.com/leadkart/leadkart-go/internal/common/clock"
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
	isGloballySuspended    bool      // rare global ban — compliance / fraud / cross-tenant abuse
	globalSuspensionReason string    // populated when isGloballySuspended; cleared on Lift
	globallySuspendedAt    time.Time // zero unless suspended; reset on Lift
	createdAt              time.Time
	anonymisedAt           time.Time
	events                 []Event
}

// New constructs a brand-new Person. Returns [ErrInvalid] (wrapped) on
// invariant violation. The aggregate emits [CreatedEvent] which the
// repository drains via [PullEvents] when persisting.
func New(id ID, e email.Address, firstName, lastName string, passwordHash PasswordHash) (*Person, error) {
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

	now := clock.Now()
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

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                     ID
	Email                  email.Address
	FirstName              string
	LastName               string
	PasswordHash           PasswordHash
	SecurityStamp          SecurityStamp
	IsActive               bool
	IsAnonymised           bool
	IsGloballySuspended    bool
	GlobalSuspensionReason string
	GloballySuspendedAt    time.Time
	CreatedAt              time.Time
	AnonymisedAt           time.Time
}

// UnmarshalFromDB re-hydrates a Person from persistence. Used ONLY by
// the repository on read paths — does NOT re-validate (TDL canon).
func UnmarshalFromDB(s Snapshot) *Person {
	return &Person{
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
		createdAt:              s.CreatedAt,
		anonymisedAt:           s.AnonymisedAt,
	}
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

// ----- State transitions ----------------------------------------------------

// ChangePassword updates the password hash AND rotates the SecurityStamp.
//
// Stamp rotation invalidates all outstanding JWTs on next request — that's
// the security guarantee per `security.md` "SecurityStamp rotation triggers".
//
// Idempotency: NOT idempotent. Each call rotates the stamp + emits an event.
func (p *Person) ChangePassword(newHash PasswordHash) error {
	if newHash.IsZero() {
		return fmt.Errorf("%w: new password hash required", ErrInvalid)
	}
	now := clock.Now()
	p.passwordHash = newHash
	p.securityStamp = NewSecurityStamp()
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
func (p *Person) UpdateProfile(firstName, lastName string) error {
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
		At:           clock.Now(),
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
func (p *Person) GloballySuspend(reason string) error {
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
	now := clock.Now()
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
func (p *Person) LiftGlobalSuspension() error {
	if !p.isGloballySuspended {
		return nil
	}
	p.isGloballySuspended = false
	p.globalSuspensionReason = ""
	p.globallySuspendedAt = time.Time{}
	p.recordEvent(GlobalSuspensionLiftedEvent{
		PersonID: p.id,
		At:       clock.Now(),
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
func (p *Person) Anonymise() error {
	if p.isAnonymised {
		return nil
	}
	now := clock.Now()
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
