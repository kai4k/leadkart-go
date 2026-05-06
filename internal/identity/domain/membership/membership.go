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
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
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

	// roleAssignments holds the IDs of every Role the Membership
	// carries. Persistence layer joins to identity.role_assignments.
	// CALLER INVARIANT (application service): every Role assigned MUST
	// belong to the same tenantID — cross-tenant role assignment is a
	// doctrine violation per `multi-tenancy.md`. The DB-level composite
	// FK on (membership_id, tenant_id) → (id, tenant_id) guards this.
	roleAssignments []role.ID

	// grantedPermissions / revokedPermissions form the per-Membership
	// overlay ON TOP of role-derived permissions. Effective set
	// (computed by Task 13's resolver):
	//
	//   union(role.Permissions for r in roleAssignments)
	//     ∪ grantedPermissions
	//     \ revokedPermissions
	//
	// The overlay supports per-user customisation without role
	// explosion (every "X but with Y disabled" doesn't need a clone of
	// X). Persistence layer projects to
	// identity.membership_permission_overrides (kind ∈ {granted, revoked}).
	grantedPermissions []*permission.Permission
	revokedPermissions []*permission.Permission

	events []Event
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
	ID                 ID
	PersonID           person.ID
	TenantID           tenant.ID
	Status             Status
	JoinedAt           time.Time
	LeftAt             time.Time
	RoleAssignments    []role.ID
	GrantedPermissions []*permission.Permission
	RevokedPermissions []*permission.Permission
}

// UnmarshalFromDB re-hydrates a Membership from persistence.
// Repository-only path; does NOT re-validate (TDL canon).
func UnmarshalFromDB(s Snapshot) *Membership {
	return &Membership{
		id:                 s.ID,
		personID:           s.PersonID,
		tenantID:           s.TenantID,
		status:             s.Status,
		joinedAt:           s.JoinedAt,
		leftAt:             s.LeftAt,
		roleAssignments:    append([]role.ID(nil), s.RoleAssignments...),
		grantedPermissions: append([]*permission.Permission(nil), s.GrantedPermissions...),
		revokedPermissions: append([]*permission.Permission(nil), s.RevokedPermissions...),
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

// RoleAssignments returns a defensive copy of the role-ID list. Mutations to
// the returned slice do NOT affect aggregate state — Role mutations go
// through [AssignRole] / [RevokeRole].
func (m *Membership) RoleAssignments() []role.ID {
	out := make([]role.ID, len(m.roleAssignments))
	copy(out, m.roleAssignments)
	return out
}

// GrantedPermissions returns a defensive copy of the per-Membership
// overlay-grant list. Effective set computed by Task 13's resolver:
// union(roles) ∪ Granted \ Revoked.
func (m *Membership) GrantedPermissions() []*permission.Permission {
	out := make([]*permission.Permission, len(m.grantedPermissions))
	copy(out, m.grantedPermissions)
	return out
}

// RevokedPermissions returns a defensive copy of the per-Membership
// overlay-revoke list — permissions taken AWAY from what the role
// union would otherwise grant.
func (m *Membership) RevokedPermissions() []*permission.Permission {
	out := make([]*permission.Permission, len(m.revokedPermissions))
	copy(out, m.revokedPermissions)
	return out
}

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

// ----- Authorisation: role assignments --------------------------------------

// AssignRole adds the supplied Role ID to the Membership's assignment
// list. Idempotent — assigning an already-assigned role is a no-op
// (no event).
//
// CALLER INVARIANT (application service): the Role's TenantID MUST
// equal this Membership's TenantID. Cross-tenant role assignment is
// a doctrine violation per `multi-tenancy.md`. The DB-level composite
// FK `(membership_id, tenant_id) → (id, tenant_id)` rejects mismatch.
func (m *Membership) AssignRole(roleID role.ID) error {
	if roleID.IsZero() {
		return fmt.Errorf("%w: roleID required", ErrInvalid)
	}
	for _, existing := range m.roleAssignments {
		if existing == roleID {
			return nil
		}
	}
	m.roleAssignments = append(m.roleAssignments, roleID)
	m.recordEvent(RoleAssignedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		RoleID:       roleID,
		At:           clock.Now(),
	})
	return nil
}

// RevokeRole removes the supplied Role ID from the Membership's
// assignment list. Idempotent — revoking a non-assigned role is a
// no-op (no event).
func (m *Membership) RevokeRole(roleID role.ID) error {
	if roleID.IsZero() {
		return fmt.Errorf("%w: roleID required", ErrInvalid)
	}
	for i, existing := range m.roleAssignments {
		if existing == roleID {
			m.roleAssignments = append(m.roleAssignments[:i], m.roleAssignments[i+1:]...)
			m.recordEvent(RoleRevokedEvent{
				MembershipID: m.id,
				PersonID:     m.personID,
				TenantID:     m.tenantID,
				RoleID:       roleID,
				At:           clock.Now(),
			})
			return nil
		}
	}
	return nil
}

// ----- Authorisation: per-Membership permission overlay ---------------------

// GrantPermission adds an overlay-grant entry. If the permission was
// previously overlay-revoked (suppressing a role-derived grant), the
// revoke entry is removed first — overlay grants and revokes never
// coexist for the same permission. Idempotent — granting an already-
// granted overlay is a no-op (no event).
func (m *Membership) GrantPermission(p *permission.Permission) error {
	if p == nil {
		return fmt.Errorf("%w: permission required", ErrInvalid)
	}
	// If currently in revoked overlay, lift the revoke first.
	for i, r := range m.revokedPermissions {
		if r.Equal(p) {
			m.revokedPermissions = append(m.revokedPermissions[:i], m.revokedPermissions[i+1:]...)
			break
		}
	}
	for _, g := range m.grantedPermissions {
		if g.Equal(p) {
			return nil
		}
	}
	m.grantedPermissions = append(m.grantedPermissions, p)
	m.recordEvent(PermissionsUpdatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		At:           clock.Now(),
	})
	return nil
}

// RevokePermission adds an overlay-revoke entry. If the permission
// was previously overlay-granted, the grant entry is removed first.
// Idempotent — revoking an already-revoked overlay is a no-op
// (no event).
func (m *Membership) RevokePermission(p *permission.Permission) error {
	if p == nil {
		return fmt.Errorf("%w: permission required", ErrInvalid)
	}
	// If currently in granted overlay, lift the grant first.
	for i, g := range m.grantedPermissions {
		if g.Equal(p) {
			m.grantedPermissions = append(m.grantedPermissions[:i], m.grantedPermissions[i+1:]...)
			break
		}
	}
	for _, r := range m.revokedPermissions {
		if r.Equal(p) {
			return nil
		}
	}
	m.revokedPermissions = append(m.revokedPermissions, p)
	m.recordEvent(PermissionsUpdatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		At:           clock.Now(),
	})
	return nil
}

// EffectivePermissions resolves the Membership's authoritative permission
// set by combining role-derived grants with the per-Membership overlay:
//
//	union(role.Permissions for r in roles)
//	  ∪ grantedPermissions
//	  \ revokedPermissions
//
// CALLER INVARIANT: `roles` must be the full set of Role aggregates
// matching `m.RoleAssignments()`. The application service's
// PermissionResolver (Task 21) loads them in bulk via
// `RoleRepository.GetByIDs(ctx, m.RoleAssignments())` before calling
// this method. The aggregate intentionally doesn't reach across
// aggregates per Vernon ch.10 — caller threads the dependency.
//
// Result is order-stable but not sorted; callers needing
// deterministic ordering (audit log diff, JWT claim emission) sort
// by `Permission.Name()` themselves. Pointer-equality on interned
// permissions makes set-membership cheap.
func (m *Membership) EffectivePermissions(roles []*role.Role) []*permission.Permission {
	set := map[*permission.Permission]struct{}{}
	for _, r := range roles {
		for _, p := range r.Permissions() {
			set[p] = struct{}{}
		}
	}
	for _, g := range m.grantedPermissions {
		set[g] = struct{}{}
	}
	for _, rev := range m.revokedPermissions {
		// Pointer-equality first (cheap for interned catalogue entries),
		// then fall back to name-equality scan for non-interned values
		// (rare — only Create-fresh paths produce them).
		if _, found := set[rev]; found {
			delete(set, rev)
			continue
		}
		for k := range set {
			if k.Equal(rev) {
				delete(set, k)
				break
			}
		}
	}
	out := make([]*permission.Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}

// ReplacePermissionOverlays sets both overlay slices atomically.
// Single PermissionsUpdatedEvent fires regardless of diff size —
// listeners care about "permissions changed for this Membership",
// not per-permission deltas.
//
// nil entries in either slice are silently dropped.
func (m *Membership) ReplacePermissionOverlays(
	granted []*permission.Permission,
	revoked []*permission.Permission,
) error {
	g := make([]*permission.Permission, 0, len(granted))
	for _, p := range granted {
		if p != nil {
			g = append(g, p)
		}
	}
	r := make([]*permission.Permission, 0, len(revoked))
	for _, p := range revoked {
		if p != nil {
			r = append(r, p)
		}
	}
	m.grantedPermissions = g
	m.revokedPermissions = r
	m.recordEvent(PermissionsUpdatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		At:           clock.Now(),
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
