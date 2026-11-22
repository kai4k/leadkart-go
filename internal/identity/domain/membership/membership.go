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
	"slices"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrInvalid is the sentinel for membership invariant violations.
var ErrInvalid = errs.New(errs.KindInvalidInput, "membership", "invalid membership")

// Per-tenant profile field caps, in UTF-8 BYTES after trimming (the unit of the
// other identity name rules: person names 100, company names 200). A stored value
// over its cap, saved before the cap existed, is accepted when re-sent unchanged.
const (
	DesignationMaxLen   = 120
	DepartmentMaxLen    = 120
	StatusMessageMaxLen = 280
)

// GrantedOverride is one entry in the per-Membership permission overlay
// GRANT slice. ExpiresAt zero = perpetual; otherwise the resolver filters
// the entry out once now >= ExpiresAt per ADR 0055.
//
// Filtering happens at resolve time (NOT via a cron sweep) — the row
// stays in the DB until re-replaced by a Membership write; the
// PermissionResolver carries the clock so tests can pin it.
type GrantedOverride struct {
	Permission *permission.Permission
	ExpiresAt  time.Time
}

// ID is the Membership primary key (UUIDv7).
type ID string

// IsZero reports whether the ID is unset.
func (i ID) IsZero() bool { return i == "" }

// ParseID canonicalises an untrusted membership id (any RFC 4122 spelling) to
// the lowercase hyphenated form repositories store (ADR 0069, finding 1).
func ParseID(raw string) (ID, error) {
	c, err := ids.Canonical(raw)
	if err != nil {
		return "", fmt.Errorf("%w: membership id %q is not a UUID", ErrInvalid, raw)
	}
	return ID(c), nil
}

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
	// (computed by [Membership.EffectivePermissions]):
	//
	//   union(role.Permissions for r in roleAssignments)
	//     ∪ grantedPermissions (filtered by ExpiresAt)
	//     \ revokedPermissions
	//
	// The overlay supports per-user customisation without role
	// explosion (every "X but with Y disabled" doesn't need a clone of
	// X). Persistence layer projects to
	// identity.membership_permission_overrides (kind ∈ {granted, revoked}).
	//
	// granted entries carry an optional ExpiresAt (zero = perpetual);
	// revoked entries do NOT (revocations are permanent until re-granted).
	// ADR 0055 — approval workflow grants time-bound overlay entries
	// per AWS STS / Microsoft Entra ID JIT-access canon.
	grantedPermissions []GrantedOverride
	revokedPermissions []*permission.Permission

	// Per-tenant profile fields. Job change → new Membership in new
	// tenant; old Membership keeps its old profile fields for audit.
	designation   string // "Sales Manager", "Regional Head", etc.
	department    string // optional grouping
	statusMessage string // free-text current-availability blurb

	// reportsTo points at this Membership's manager Membership ID;
	// zero = top-of-tree. Self-reference rejected at the boundary.
	// Cycle detection (A reports to B, B reports to A, etc.) lives in
	// the application service since it requires loading other rows —
	// the domain only enforces the not-self invariant.
	reportsTo ID

	// createdBy is the audit chain — the Membership that created this
	// row. Zero value = system-bootstrapped (RegisterTenant first
	// admin, SuperAdmin via cmd/bootstrap). Set ONCE at construction;
	// immutable for the row's lifetime. Distinct from reportsTo (which
	// is the org-chart hierarchy + mutable). Per migration
	// 20260507000008.
	createdBy ID

	events []Event
}

// New constructs a brand-new TenantMembership in [StatusActive].
//
// createdBy carries the audit chain — the Membership that invited /
// created this user. Pass the zero ID for system-bootstrapped paths
// (RegisterTenant first admin, SuperAdmin via cmd/bootstrap); pass
// the caller's MembershipID for invited / created users.
//
// Returns [ErrInvalid] (wrapped) on invariant violation. The aggregate
// emits [CreatedEvent] which the repository drains via [PullEvents] when
// persisting + appends to the outbox same-tx (per ADR 0004 + ADR 0008).
func New(id ID, personID person.ID, tenantID tenant.ID, createdBy ID, now time.Time) (*Membership, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if personID.IsZero() {
		return nil, fmt.Errorf("%w: personID required", ErrInvalid)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalid)
	}
	// Self-creation makes no sense — a row can't be its own creator.
	// (Sentinel zero ID for bootstrap rows is the right NULL signal.)
	if !createdBy.IsZero() && createdBy == id {
		return nil, fmt.Errorf("%w: createdBy cannot equal id", ErrInvalid)
	}

	now = now.UTC()
	m := &Membership{
		id:        id,
		personID:  personID,
		tenantID:  tenantID,
		status:    StatusActive,
		joinedAt:  now,
		createdBy: createdBy,
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
//
// GrantedPermissions carries the time-bound overlay shape per ADR 0055.
// Each entry's ExpiresAt is zero for perpetual grants (default), set for
// approval-workflow-issued time-bound grants.
type Snapshot struct {
	ID                 ID
	PersonID           person.ID
	TenantID           tenant.ID
	Status             Status
	JoinedAt           time.Time
	LeftAt             time.Time
	RoleAssignments    []role.ID
	GrantedPermissions []GrantedOverride
	RevokedPermissions []*permission.Permission
	Designation        string
	Department         string
	StatusMessage      string
	ReportsTo          ID
	CreatedBy          ID // audit chain (zero = system-bootstrapped)
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
		grantedPermissions: append([]GrantedOverride(nil), s.GrantedPermissions...),
		revokedPermissions: append([]*permission.Permission(nil), s.RevokedPermissions...),
		designation:        s.Designation,
		department:         s.Department,
		statusMessage:      s.StatusMessage,
		reportsTo:          s.ReportsTo,
		createdBy:          s.CreatedBy,
	}
}

// CreatedBy returns the Membership that invited / created this row.
// Zero ID = system-bootstrapped (RegisterTenant first admin,
// SuperAdmin via cmd/bootstrap). Distinct from ReportsTo (org-chart
// hierarchy, mutable) — createdBy is the immutable audit fact.
func (m *Membership) CreatedBy() ID { return m.createdBy }

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
// overlay-grant entries (permission + ExpiresAt). Effective set computed
// by [Membership.EffectivePermissions]:
// union(roles) ∪ unexpired(Granted) \ Revoked.
func (m *Membership) GrantedPermissions() []GrantedOverride {
	out := make([]GrantedOverride, len(m.grantedPermissions))
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

// Designation returns the per-tenant job-title for this Membership.
func (m *Membership) Designation() string { return m.designation }

// Department returns the per-tenant department grouping.
func (m *Membership) Department() string { return m.department }

// StatusMessage returns the free-text current-availability blurb.
func (m *Membership) StatusMessage() string { return m.statusMessage }

// ReportsTo returns the manager Membership ID; zero = top-of-tree.
func (m *Membership) ReportsTo() ID { return m.reportsTo }

// ----- State transitions ----------------------------------------------------

// Deactivate transitions the Membership to [StatusInactive].
//
// Triggers: tenant admin removes user; user resigns; job change.
// Reason MUST be non-empty (audit requirement per `data-retention.md`).
//
// Idempotent — second Deactivate on already-inactive Membership is no-op.
func (m *Membership) Deactivate(reason string, now time.Time) error {
	reason = strings.TrimSpace(reason) // stored and emitted trimmed
	if reason == "" {
		return fmt.Errorf("%w: deactivation reason required for audit", ErrInvalid)
	}
	if err := humantext.Check("reason", reason); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if m.status == StatusInactive {
		return nil
	}
	now = now.UTC()
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
func (m *Membership) Reactivate(now time.Time) error {
	if m.status == StatusActive {
		return nil
	}
	now = now.UTC()
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
func (m *Membership) AssignRole(roleID role.ID, now time.Time) error {
	canonical, err := role.ParseID(string(roleID))
	if err != nil {
		return fmt.Errorf("%w: roleID is not a UUID", ErrInvalid)
	}
	roleID = canonical
	if slices.Contains(m.roleAssignments, roleID) {
		return nil
	}
	m.roleAssignments = append(m.roleAssignments, roleID)
	m.recordEvent(RoleAssignedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		RoleID:       roleID,
		At:           now.UTC(),
	})
	return nil
}

// RevokeRole removes the supplied Role ID from the Membership's
// assignment list. Idempotent — revoking a non-assigned role is a
// no-op (no event).
func (m *Membership) RevokeRole(roleID role.ID, now time.Time) error {
	canonical, err := role.ParseID(string(roleID))
	if err != nil {
		return fmt.Errorf("%w: roleID is not a UUID", ErrInvalid)
	}
	roleID = canonical
	idx := slices.Index(m.roleAssignments, roleID)
	if idx < 0 {
		return nil
	}
	m.roleAssignments = slices.Delete(m.roleAssignments, idx, idx+1)
	m.recordEvent(RoleRevokedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		RoleID:       roleID,
		At:           now.UTC(),
	})
	return nil
}

// ----- Authorisation: per-Membership permission overlay ---------------------

// GrantPermission adds an overlay-grant entry with an optional expiry.
// expiresAt zero = perpetual; otherwise the resolver filters it out
// after the timestamp. ADR 0055 — approval-workflow grants are time-
// bound (AWS STS / Microsoft Entra ID JIT-access canon); the existing
// "forever" admin path passes time.Time{}.
//
// If the permission was previously overlay-revoked (suppressing a
// role-derived grant), the revoke entry is removed first — overlay
// grants and revokes never coexist for the same permission. If an
// overlay-grant already exists, this call REPLACES its ExpiresAt
// (lets approval workflows refresh / shorten an existing grant) and
// emits PermissionsUpdatedEvent so cache invalidators trigger.
// True idempotence (same permission + identical ExpiresAt) emits no
// event.
func (m *Membership) GrantPermission(p *permission.Permission, expiresAt time.Time, now time.Time) error {
	if p == nil {
		return fmt.Errorf("%w: permission required", ErrInvalid)
	}
	// If currently in revoked overlay, lift the revoke first.
	if i := slices.IndexFunc(m.revokedPermissions, p.Equal); i >= 0 {
		m.revokedPermissions = slices.Delete(m.revokedPermissions, i, i+1)
	}
	if i := slices.IndexFunc(m.grantedPermissions, func(g GrantedOverride) bool {
		return g.Permission.Equal(p)
	}); i >= 0 {
		// True no-op when ExpiresAt unchanged.
		if m.grantedPermissions[i].ExpiresAt.Equal(expiresAt) {
			return nil
		}
		m.grantedPermissions[i].ExpiresAt = expiresAt
		m.recordEvent(PermissionsUpdatedEvent{
			MembershipID: m.id,
			PersonID:     m.personID,
			TenantID:     m.tenantID,
			At:           now.UTC(),
		})
		return nil
	}
	m.grantedPermissions = append(m.grantedPermissions, GrantedOverride{Permission: p, ExpiresAt: expiresAt})
	m.recordEvent(PermissionsUpdatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		At:           now.UTC(),
	})
	return nil
}

// RevokePermission adds an overlay-revoke entry. If the permission
// was previously overlay-granted, the grant entry is removed first.
// Idempotent — revoking an already-revoked overlay is a no-op
// (no event).
func (m *Membership) RevokePermission(p *permission.Permission, now time.Time) error {
	if p == nil {
		return fmt.Errorf("%w: permission required", ErrInvalid)
	}
	// If currently in granted overlay, lift the grant first.
	if i := slices.IndexFunc(m.grantedPermissions, func(g GrantedOverride) bool {
		return g.Permission.Equal(p)
	}); i >= 0 {
		m.grantedPermissions = slices.Delete(m.grantedPermissions, i, i+1)
	}
	if slices.ContainsFunc(m.revokedPermissions, p.Equal) {
		return nil
	}
	m.revokedPermissions = append(m.revokedPermissions, p)
	m.recordEvent(PermissionsUpdatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		At:           now.UTC(),
	})
	return nil
}

// EffectivePermissions resolves the Membership's authoritative permission
// set by combining role-derived grants with the per-Membership overlay:
//
//	union(role.Permissions for r in roles)
//	  ∪ unexpired(grantedPermissions, now)
//	  \ revokedPermissions
//
// CALLER INVARIANT: `roles` must be the full set of Role aggregates
// matching `m.RoleAssignments()`. The application service's
// PermissionResolver (Task 21) loads them in bulk via
// `RoleRepository.GetByIDs(ctx, m.RoleAssignments())` before calling
// this method. The aggregate intentionally doesn't reach across
// aggregates per Vernon ch.10 — caller threads the dependency.
//
// now is the wall-clock used to filter expired overlay grants per
// ADR 0055. Pass the resolver-injected time source in production
// (composition root wires `time.Now`); tests inject a fixed instant
// closure for deterministic assertions. Time-bound grants whose
// ExpiresAt is in the past are simply dropped from the set — no DB
// cleanup runs at this layer.
//
// Result is order-stable but not sorted; callers needing
// deterministic ordering (audit log diff, JWT claim emission) sort
// by `Permission.Name()` themselves. Pointer-equality on interned
// permissions makes set-membership cheap.
func (m *Membership) EffectivePermissions(roles []*role.Role, now time.Time) []*permission.Permission {
	set := map[*permission.Permission]struct{}{}
	for _, r := range roles {
		for _, p := range r.Permissions() {
			set[p] = struct{}{}
		}
	}
	for _, g := range m.grantedPermissions {
		if !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt) {
			continue // expired — resolver-time filtered per ADR 0055
		}
		set[g.Permission] = struct{}{}
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

// ReplacePermissionOverrides sets both overlay lists to EXACTLY what the caller
// decided on: each granted entry carries its expiry as approved (zero =
// permanent) and no liveness is recomputed here, so the instant the policy
// snapshotted at and the state written cannot diverge. nil permissions are
// dropped; a name listed twice keeps its first entry (granted and revoked
// alike). A name in BOTH lists is contradictory: ErrInvalid, nothing changes.
func (m *Membership) ReplacePermissionOverrides(
	granted []GrantedOverride,
	revoked []*permission.Permission,
	now time.Time,
) error {
	g := make([]GrantedOverride, 0, len(granted))
	seen := make(map[string]struct{}, len(granted))
	for _, e := range granted {
		if e.Permission == nil {
			continue
		}
		if _, dup := seen[e.Permission.Name()]; dup {
			continue
		}
		seen[e.Permission.Name()] = struct{}{}
		g = append(g, e)
	}
	r := make([]*permission.Permission, 0, len(revoked))
	seenRevoked := make(map[string]struct{}, len(revoked))
	for _, p := range revoked {
		if p == nil {
			continue
		}
		if _, dup := seenRevoked[p.Name()]; dup {
			continue
		}
		if _, both := seen[p.Name()]; both {
			return fmt.Errorf("%w: permission %q is both granted and revoked", ErrInvalid, p.Name())
		}
		seenRevoked[p.Name()] = struct{}{}
		r = append(r, p)
	}
	m.grantedPermissions = g
	m.revokedPermissions = r
	m.recordEvent(PermissionsUpdatedEvent{
		MembershipID: m.id,
		PersonID:     m.personID,
		TenantID:     m.tenantID,
		At:           now.UTC(),
	})
	return nil
}

// ----- Per-tenant profile + manager hierarchy --------------------------------

// UpdateProfile sets `designation` / `department` / `statusMessage`
// atomically. Whitespace trimmed on all three. Idempotent: trimmed
// values matching the current state emit no event.
//
// Single ProfileUpdatedEvent fires regardless of which fields
// changed — listeners care about "profile changed" not per-field
// deltas.
func (m *Membership) UpdateProfile(designation, department, statusMessage string, now time.Time) error {
	d := strings.TrimSpace(designation)
	dep := strings.TrimSpace(department)
	sm := strings.TrimSpace(statusMessage)
	// A full replace re-sends the stored values: only a CHANGED field is checked, so a
	// value stored before the rule existed never blocks an untouched save.
	for _, f := range []struct {
		field, stored, next string
		max                 int
	}{
		{"designation", m.designation, d, DesignationMaxLen},
		{"department", m.department, dep, DepartmentMaxLen},
		{"status_message", m.statusMessage, sm, StatusMessageMaxLen},
	} {
		// The cap is measured on the trimmed value, in UTF-8 bytes like every other
		// identity name rule; a value that is not being changed is never re-measured.
		if f.next != f.stored && len(f.next) > f.max {
			return fmt.Errorf("%w: %w", ErrInvalid, humantext.TooLong(f.field))
		}
		if err := humantext.CheckChanged(f.field, f.stored, f.next); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if d == m.designation && dep == m.department && sm == m.statusMessage {
		return nil
	}
	m.designation = d
	m.department = dep
	m.statusMessage = sm
	m.recordEvent(ProfileUpdatedEvent{
		MembershipID:  m.id,
		PersonID:      m.personID,
		TenantID:      m.tenantID,
		Designation:   d,
		Department:    dep,
		StatusMessage: sm,
		At:            now.UTC(),
	})
	return nil
}

// AssignManager sets the `reportsTo` field. Pass zero ID to clear
// (top-of-tree). Self-reference rejected. Idempotent — assigning the
// current manager is a no-op (no event).
//
// CALLER INVARIANT: the application service runs cycle detection
// (recursive CTE on the persisted hierarchy) before calling this.
// The domain doesn't traverse other Memberships.
//
// Manager Membership MUST belong to the same tenant — DB-level FK on
// `(reports_to, tenant_id) → (id, tenant_id)` enforces this; domain
// trusts the boundary.
//
// Emits ManagerAssignedEvent on set (with PreviousManager carried for
// audit), ManagerRemovedEvent on clear.
func (m *Membership) AssignManager(managerID ID, now time.Time) error {
	if !managerID.IsZero() {
		canonical, err := ParseID(string(managerID))
		if err != nil {
			return err
		}
		managerID = canonical
	}
	self, err := ParseID(string(m.id))
	if err != nil {
		return err
	}
	if managerID == self {
		return fmt.Errorf("%w: cannot report to self", ErrInvalid)
	}
	if m.reportsTo == managerID {
		return nil
	}
	old := m.reportsTo
	m.reportsTo = managerID
	at := now.UTC()
	if managerID.IsZero() {
		m.recordEvent(ManagerRemovedEvent{
			MembershipID:    m.id,
			PersonID:        m.personID,
			TenantID:        m.tenantID,
			PreviousManager: old,
			At:              at,
		})
		return nil
	}
	m.recordEvent(ManagerAssignedEvent{
		MembershipID:    m.id,
		PersonID:        m.personID,
		TenantID:        m.tenantID,
		ManagerID:       managerID,
		PreviousManager: old,
		At:              at,
	})
	return nil
}

// RemoveManager clears the `reportsTo` field — semantic-clear inverse
// of [Membership.AssignManager]. Idempotent: calling on an already-
// cleared Membership is a no-op (no event). When clearing an actual
// previous manager, emits ManagerRemovedEvent carrying the
// PreviousManager for downstream audit / hierarchy-projection updates.
//
// Mirrors the .NET LeadKart MembershipManager remove command per
// messaging.md "Identity event vocabulary" — keeps the read site free
// of the "AssignManager(zero)" idiom which reads as a smell.
func (m *Membership) RemoveManager(now time.Time) error {
	return m.AssignManager(ID(""), now)
}

// ----- Event handling -------------------------------------------------------

// HasPendingEvents reports whether the aggregate recorded a change since it was
// loaded (without draining the events). Idempotent no-ops record none, so a
// command can tell "something changed" from "already so".
func (m *Membership) HasPendingEvents() bool { return len(m.events) > 0 }

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
