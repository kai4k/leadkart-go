// Package tenant defines the Tenant aggregate (ADR 0002, multi-tenancy.md).
//
// Tenant is not itself tenant-scoped — each row IS a tenant. Identified by
// [ID] (UUIDv7) and addressed by [Slug]. Construction is via [New] (invariant
// enforcement) or [UnmarshalFromDB] (repository re-hydration, no re-validation).
package tenant

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// ErrInvalid is returned (wrapped via %w) on invariant violation.
// Callers branch via [errors.Is].
var ErrInvalid = errs.New(errs.KindInvalidInput, "tenant", "invalid tenant")

// PlatformSlug is the canonical slug of the LeadKart platform tenant.
// Both the login command and platform-tier middleware anchor on this constant
// so they can never disagree about which tenant is privileged.
const PlatformSlug = "platform"

// ID is the tenant primary key (UUIDv7 string). Wrapper type prevents
// accidental swap with other domain ID types.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// ParseID canonicalises an untrusted tenant id (any RFC 4122 spelling) to the
// lowercase hyphenated form repositories store; whitespace, empty and the nil
// UUID are rejected (ADR 0069, finding 1).
func ParseID(raw string) (ID, error) {
	c, err := ids.Canonical(raw)
	if err != nil {
		return "", fmt.Errorf("%w: tenant id %q is not a UUID", ErrInvalid, raw)
	}
	return ID(c), nil
}

// nameMaxLen caps legal and display names to keep audit logs and UI predictable.
const nameMaxLen = 200

// Tenant is the aggregate root. Invariants enforced by [New] and state
// transition methods: ID and Slug non-zero; LegalName and DisplayName
// non-empty and ≤ nameMaxLen; Status follows the documented state machine.
//
// Admin email is NOT stored here (migration 20260507000008). It is a derived
// value — query the CompanyOwner-role membership. Storing it here would create
// a second source of truth that silently drifts after email-change flows.
type Tenant struct {
	id                 ID
	slug               slug.Slug
	legalName          string
	displayName        string
	status             Status
	statutory          Statutory          // optional Indian statutory IDs (GST/PAN/DrugLicence)
	adminContact       AdminContact       // optional admin phone + postal address
	settings           Settings           // password policy + future operational settings
	displayPreferences DisplayPreferences // locale / time zone / date format / currency
	createdAt          time.Time
	activatedAt        time.Time // zero until first Activate
	suspendedAt        time.Time // zero until first Suspend; reset on subsequent Activate
	events             []Event
}

// New constructs a brand-new Tenant in [StatusPending] and emits a
// [RegisteredEvent]. adminEmail is required for the event payload (welcome-
// email subscribers need it as a point-in-time fact) but is NOT stored on the
// aggregate (migration 20260507000008). Returns [ErrInvalid] on invariant
// violation.
func New(id ID, s slug.Slug, legalName, displayName string, adminEmail email.Address, now time.Time) (*Tenant, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if s.IsZero() {
		return nil, fmt.Errorf("%w: slug required", ErrInvalid)
	}
	// Names are trimmed first, so the limit is measured on what is stored.
	legalName, displayName = strings.TrimSpace(legalName), strings.TrimSpace(displayName)
	if legalName == "" {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, humantext.Required("legal_name"))
	}
	if displayName == "" {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, humantext.Required("display_name"))
	}
	if utf8.RuneCountInString(legalName) > nameMaxLen {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, humantext.TooLong("legal_name"))
	}
	if utf8.RuneCountInString(displayName) > nameMaxLen {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, humantext.TooLong("display_name"))
	}
	if err := humantext.Check("legal_name", legalName); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := humantext.Check("display_name", displayName); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if adminEmail.IsZero() {
		return nil, fmt.Errorf("%w: admin email required for RegisteredEvent payload", ErrInvalid)
	}

	now = now.UTC()
	t := &Tenant{
		id:          id,
		slug:        s,
		legalName:   legalName,
		displayName: displayName,
		status:      StatusPending,
		createdAt:   now,
	}
	t.recordEvent(RegisteredEvent{
		TenantID:    id,
		Slug:        s,
		LegalName:   legalName,
		DisplayName: displayName,
		AdminEmail:  adminEmail,
		At:          now,
	})
	return t, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
// The adapter scans DB rows into this struct, keeping it free of internal
// Tenant field knowledge.
type Snapshot struct {
	ID                 ID
	Slug               slug.Slug
	LegalName          string
	DisplayName        string
	Status             Status
	Statutory          Statutory
	AdminContact       AdminContact
	Settings           Settings
	DisplayPreferences DisplayPreferences
	CreatedAt          time.Time
	ActivatedAt        time.Time
	SuspendedAt        time.Time
}

// UnmarshalFromDB re-hydrates a Tenant from persistence. Used only by the
// repository; does not re-validate invariants. Per TDL canon: data valid when
// stored must survive even if invariants later tighten.
func UnmarshalFromDB(s Snapshot) *Tenant {
	return &Tenant{
		id:                 s.ID,
		slug:               s.Slug,
		legalName:          s.LegalName,
		displayName:        s.DisplayName,
		status:             s.Status,
		statutory:          s.Statutory,
		adminContact:       s.AdminContact,
		settings:           s.Settings,
		displayPreferences: s.DisplayPreferences,
		createdAt:          s.CreatedAt,
		activatedAt:        s.ActivatedAt,
		suspendedAt:        s.SuspendedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the tenant primary key.
func (t *Tenant) ID() ID { return t.id }

// Slug returns the URL-safe public handle.
func (t *Tenant) Slug() slug.Slug { return t.slug }

// LegalName returns the registered legal name.
func (t *Tenant) LegalName() string { return t.legalName }

// DisplayName returns the UI-facing friendly name.
func (t *Tenant) DisplayName() string { return t.displayName }

// Status returns the current lifecycle state.
func (t *Tenant) Status() Status { return t.status }

// MembersMaySignIn reports whether the tenant's members may open or keep a session
// (see [Status.MembersMaySignIn] for the per-status answers and the reasons).
func (t *Tenant) MembersMaySignIn() bool { return t.status.MembersMaySignIn() }

// CreatedAt returns the registration timestamp.
func (t *Tenant) CreatedAt() time.Time { return t.createdAt }

// ActivatedAt returns the most recent activation timestamp; zero if never activated.
func (t *Tenant) ActivatedAt() time.Time { return t.activatedAt }

// SuspendedAt returns the most recent suspension timestamp; zero if never suspended.
func (t *Tenant) SuspendedAt() time.Time { return t.suspendedAt }

// Statutory returns declared statutory IDs; zero means none declared yet.
func (t *Tenant) Statutory() Statutory { return t.statutory }

// AdminContact returns the admin phone + postal address; zero means not declared.
func (t *Tenant) AdminContact() AdminContact { return t.adminContact }

// Settings returns operational settings; zero means uninitialised — treat as
// DefaultPasswordPolicy at runtime.
func (t *Tenant) Settings() Settings { return t.settings }

// DisplayPreferences returns UI rendering preferences; zero means uninitialised —
// treat as DefaultDisplayPreferences at runtime.
func (t *Tenant) DisplayPreferences() DisplayPreferences { return t.displayPreferences }

// ----- State transitions ----------------------------------------------------

// UpdateProfile changes LegalName and DisplayName. Each attribute type has its
// own mutator; narrow events let subscribers react to exactly what changed.
// Idempotent: no event when both values are unchanged. Allowed in any status —
// suspended tenants may rename for legal reasons without re-activating.
func (t *Tenant) UpdateProfile(legalName, displayName string, now time.Time) error {
	// A full replace re-sends the stored values: a value equal to the stored one (as
	// stored, or once both are trimmed: a name saved before names were trimmed) is
	// unchanged and never re-checked, so an untouched save is always accepted.
	legalSame := legalName == t.legalName || strings.TrimSpace(legalName) == strings.TrimSpace(t.legalName)
	displaySame := displayName == t.displayName || strings.TrimSpace(displayName) == strings.TrimSpace(t.displayName)
	// Names are trimmed first, so the limit is measured on what is stored.
	legalName, displayName = strings.TrimSpace(legalName), strings.TrimSpace(displayName)
	if legalName == "" {
		return fmt.Errorf("%w: %w", ErrInvalid, humantext.Required("legal_name"))
	}
	if displayName == "" {
		return fmt.Errorf("%w: %w", ErrInvalid, humantext.Required("display_name"))
	}
	if utf8.RuneCountInString(legalName) > nameMaxLen && !legalSame {
		return fmt.Errorf("%w: %w", ErrInvalid, humantext.TooLong("legal_name"))
	}
	if utf8.RuneCountInString(displayName) > nameMaxLen && !displaySame {
		return fmt.Errorf("%w: %w", ErrInvalid, humantext.TooLong("display_name"))
	}
	if !legalSame {
		if err := humantext.Check("legal_name", legalName); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if !displaySame {
		if err := humantext.Check("display_name", displayName); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if legalSame && displaySame {
		return nil
	}
	// A real change stores both names trimmed.
	old := struct{ legal, display string }{t.legalName, t.displayName}
	t.legalName = legalName
	t.displayName = displayName
	t.recordEvent(ProfileUpdatedEvent{
		TenantID:       t.id,
		OldLegalName:   old.legal,
		OldDisplayName: old.display,
		NewLegalName:   legalName,
		NewDisplayName: displayName,
		At:             now.UTC(),
	})
	return nil
}

// Activate transitions the tenant to [StatusActive]. Idempotent if already
// active. Emits [ActivatedEvent] and records the activation timestamp.
func (t *Tenant) Activate(now time.Time) error {
	if t.status == StatusActive {
		return nil // idempotent — TDL canon for already-correct state
	}
	now = now.UTC()
	t.status = StatusActive
	t.activatedAt = now
	t.recordEvent(ActivatedEvent{
		TenantID: t.id,
		At:       now,
	})
	return nil
}

// Suspend transitions the tenant to [StatusSuspended]. reason must be
// non-empty (audit requirement, data-retention.md). Idempotent if already
// suspended.
func (t *Tenant) Suspend(reason string, now time.Time) error {
	reason = strings.TrimSpace(reason) // stored and emitted trimmed
	if reason == "" {
		return fmt.Errorf("%w: suspension reason required for audit", ErrInvalid)
	}
	if err := humantext.Check("reason", reason); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if t.status == StatusSuspended {
		return nil
	}
	now = now.UTC()
	t.status = StatusSuspended
	t.suspendedAt = now
	t.recordEvent(SuspendedEvent{
		TenantID: t.id,
		Reason:   reason,
		At:       now,
	})
	return nil
}

// UpdateStatutory replaces declared Indian statutory IDs with the supplied
// [Statutory] composite. Pass zero to clear all declarations. Validation
// (GST+PAN cross-check) already ran in [NewStatutory]; this method only
// enforces lifecycle rules. Idempotent when value is unchanged.
func (t *Tenant) UpdateStatutory(s Statutory, now time.Time) error {
	if t.statutory.Equal(s) {
		return nil
	}
	old := t.statutory
	t.statutory = s
	t.recordEvent(StatutoryUpdatedEvent{
		TenantID:     t.id,
		OldStatutory: old,
		NewStatutory: s,
		At:           now.UTC(),
	})
	return nil
}

// UpdateAdminContact replaces the admin phone + postal address. Pass zero to
// clear. Validation ran in the VO constructors; this method only enforces
// lifecycle rules. Idempotent when value is unchanged.
func (t *Tenant) UpdateAdminContact(c AdminContact, now time.Time) error {
	if t.adminContact.Equal(c) {
		return nil
	}
	old := t.adminContact
	t.adminContact = c
	t.recordEvent(AdminContactUpdatedEvent{
		TenantID:        t.id,
		OldAdminContact: old,
		NewAdminContact: c,
		At:              now.UTC(),
	})
	return nil
}

// UpdateSettings replaces operational [Settings]. Validation ran in
// [NewPasswordPolicy]. Idempotent when value is unchanged. Auth caches must react to [SettingsUpdatedEvent] to
// invalidate cached password policy.
func (t *Tenant) UpdateSettings(s Settings, now time.Time) error {
	if t.settings.Equal(s) {
		return nil
	}
	old := t.settings
	t.settings = s
	t.recordEvent(SettingsUpdatedEvent{
		TenantID:    t.id,
		OldSettings: old,
		NewSettings: s,
		At:          now.UTC(),
	})
	return nil
}

// UpdateDisplayPreferences replaces UI rendering preferences. Validation ran in
// [NewDisplayPreferences]. Idempotent when value is unchanged. Subscribers (BFF, notification renderers) must invalidate
// cached preferences on receipt.
func (t *Tenant) UpdateDisplayPreferences(d DisplayPreferences, now time.Time) error {
	if t.displayPreferences.Equal(d) {
		return nil
	}
	old := t.displayPreferences
	t.displayPreferences = d
	t.recordEvent(DisplayPreferencesUpdatedEvent{
		TenantID:              t.id,
		OldDisplayPreferences: old,
		NewDisplayPreferences: d,
		At:                    now.UTC(),
	})
	return nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains and returns recorded domain events. The repository calls
// this inside the same transaction that persists state, then writes events to
// the outbox. Clears the internal slice; subsequent calls return nil until
// new mutations record events.
func (t *Tenant) PullEvents() []Event {
	if len(t.events) == 0 {
		return nil
	}
	out := t.events
	t.events = nil
	return out
}

func (t *Tenant) recordEvent(e Event) {
	t.events = append(t.events, e)
}
