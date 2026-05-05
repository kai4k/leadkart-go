// Package membership defines the TenantMembership aggregate — the per-tenant
// junction between [person.Person] (global identity) and [tenant.Tenant]
// (the workspace).
//
// Architectural context (per LeadKart .NET `multi-tenancy.md` "Identity model"):
//
// LeadKart follows the Auth0 / Microsoft Entra ID / Slack / Linear / Stripe
// canonical pattern: one global Person can hold many Memberships across
// tenants over time, but at any moment AT MOST ONE Membership is Active
// (DB-enforced via partial unique index `WHERE status = 'Active' AND NOT
// is_deleted`).
//
// This split allows:
//   - Same human switches tenants over time (job change → deactivate old
//     Membership, create new Active Membership).
//   - Personal email reuse across tenants (Person.Email is globally unique;
//     Membership owns per-tenant context).
//   - SuperUser god-mode without tenant-membership pollution (separate flag
//     on Membership in the Platform tenant).
//
// Status state machine: Active ↔ Inactive. (No Pending state — Memberships
// are created Active when a tenant admin onboards a user; verification flow
// happens at Person/Email level upstream.)
package membership

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrInvalid is the sentinel for membership invariant violations.
var ErrInvalid = errs.New(errs.KindInvalidInput, "membership", "invalid membership")

// ID is the Membership primary key (UUIDv7).
type ID string

// IsZero reports whether the ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Membership is the aggregate root.
//
// Invariants:
//   - ID, PersonID, TenantID are all non-zero.
//   - Status follows Active ↔ Inactive transitions.
//   - JoinedAt set at creation; LeftAt set on deactivation, cleared on
//     reactivation.
type Membership struct {
	id       ID
	personID person.ID
	tenantID tenant.ID
	status   Status
	joinedAt time.Time
	leftAt   time.Time // zero unless inactive
	events   []Event
}

// New constructs a brand-new TenantMembership in [StatusActive].
//
// Returns [ErrInvalid] (wrapped) on invariant violation. The aggregate
// emits [CreatedEvent] which the repository drains via [PullEvents] when
// persisting + appends to the outbox same-tx (per ADR 0004 + ADR 0008).
func New(id ID, personID person.ID, tenantID tenant.ID) (*Membership, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if personID.IsZero() {
		return nil, fmt.Errorf("%w: personID required", ErrInvalid)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalid)
	}

	now := clock.Now()
	m := &Membership{
		id:       id,
		personID: personID,
		tenantID: tenantID,
		status:   StatusActive,
		joinedAt: now,
	}
	m.recordEvent(CreatedEvent{
		MembershipID: id,
		PersonID:     personID,
		TenantID:     tenantID,
		At:           now,
	})
	return m, nil
}

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID       ID
	PersonID person.ID
	TenantID tenant.ID
	Status   Status
	JoinedAt time.Time
	LeftAt   time.Time
}

// UnmarshalFromDB re-hydrates a Membership from persistence.
// Repository-only path; does NOT re-validate (TDL canon).
func UnmarshalFromDB(s Snapshot) *Membership {
	return &Membership{
		id:       s.ID,
		personID: s.PersonID,
		tenantID: s.TenantID,
		status:   s.Status,
		joinedAt: s.JoinedAt,
		leftAt:   s.LeftAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the Membership primary key.
func (m *Membership) ID() ID { return m.id }

// PersonID returns the FK to [person.Person].
func (m *Membership) PersonID() person.ID { return m.personID }

// TenantID returns the FK to [tenant.Tenant].
func (m *Membership) TenantID() tenant.ID { return m.tenantID }

// Status returns the current Active/Inactive state.
func (m *Membership) Status() Status { return m.status }

// JoinedAt returns the timestamp when the Membership was created.
func (m *Membership) JoinedAt() time.Time { return m.joinedAt }

// LeftAt returns the most recent deactivation timestamp; zero if currently active.
func (m *Membership) LeftAt() time.Time { return m.leftAt }

// ----- State transitions ----------------------------------------------------

// Deactivate transitions the Membership to [StatusInactive].
//
// Triggers: tenant admin removes user; user resigns; job change.
// Reason MUST be non-empty (audit requirement per `data-retention.md`).
//
// Idempotent — second Deactivate on already-inactive Membership is no-op.
func (m *Membership) Deactivate(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: deactivation reason required for audit", ErrInvalid)
	}
	if m.status == StatusInactive {
		return nil
	}
	now := clock.Now()
	m.status = StatusInactive
	m.leftAt = now
	m.recordEvent(DeactivatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		Reason:       reason,
		At:           now,
	})
	return nil
}

// Reactivate transitions the Membership back to [StatusActive].
//
// LeftAt is cleared (zero) on reactivation per `multi-tenancy.md` doctrine —
// the Membership "rejoins" the live set; LeftAt only carries meaning for
// inactive Memberships.
//
// Idempotent — second Reactivate on already-active Membership is no-op.
//
// CALLER INVARIANT: the application service MUST verify the Person has no
// other Active Membership before calling Reactivate (the DB partial unique
// index will reject otherwise; surface as ErrAlreadyActive in the service).
func (m *Membership) Reactivate() error {
	if m.status == StatusActive {
		return nil
	}
	now := clock.Now()
	m.status = StatusActive
	m.leftAt = time.Time{} // clear
	m.recordEvent(ReactivatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		At:           now,
	})
	return nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains recorded events. See [tenant.Tenant.PullEvents] for semantics.
func (m *Membership) PullEvents() []Event {
	if len(m.events) == 0 {
		return nil
	}
	out := m.events
	m.events = nil
	return out
}

func (m *Membership) recordEvent(e Event) {
	m.events = append(m.events, e)
}
