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
	id            ID
	email         email.Address
	firstName     string
	lastName      string
	passwordHash  PasswordHash
	securityStamp SecurityStamp
	isActive      bool
	isAnonymised  bool
	createdAt     time.Time
	anonymisedAt  time.Time
	events        []Event
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
	ID            ID
	Email         email.Address
	FirstName     string
	LastName      string
	PasswordHash  PasswordHash
	SecurityStamp SecurityStamp
	IsActive      bool
	IsAnonymised  bool
	CreatedAt     time.Time
	AnonymisedAt  time.Time
}

// UnmarshalFromDB re-hydrates a Person from persistence. Used ONLY by
// the repository on read paths — does NOT re-validate (TDL canon).
func UnmarshalFromDB(s Snapshot) *Person {
	return &Person{
		id:            s.ID,
		email:         s.Email,
		firstName:     s.FirstName,
		lastName:      s.LastName,
		passwordHash:  s.PasswordHash,
		securityStamp: s.SecurityStamp,
		isActive:      s.IsActive,
		isAnonymised:  s.IsAnonymised,
		createdAt:     s.CreatedAt,
		anonymisedAt:  s.AnonymisedAt,
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
