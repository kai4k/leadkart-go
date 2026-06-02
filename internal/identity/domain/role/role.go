// Package role defines the Role aggregate — a tenant-scoped named
// bundle of [permission.Permission]s. Mirrors .NET LeadKart's
// `Modules.Identity.Domain.Roles.Role`: per-tenant catalog, system-
// default roles refuse mutation, soft-delete with audit, hierarchy
// level drives `IUserHierarchyQueries` (Phase 7).
//
// Hierarchy (parent→child organizational tree) is owned by the
// SEPARATE [rolehierarchy] aggregate per ADR 0058 (Wave 9.4 —
// supersedes ADR 0054). The Role aggregate stays focused on its own
// state (name, permissions, hierarchy_level, soft-delete). The
// previous `parent_role_id` field + its `ChangeParent` method moved
// out wholesale.
package role

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Hierarchy-level constants. Lower = higher authority. 0 = top
// (Owner), 99 = bottom OR sentinel for "no role assigned".
const (
	HierarchyLevelDefault = 50
	HierarchyLevelMin     = 0
	HierarchyLevelMax     = 99
	HierarchyLevelNoRole  = 99
)

// Role.Name length bounds. Mirror of the .NET parent's Role.cs
// Create / Rename validators. Exported so admin UI + DTO validation
// can reuse the same numbers without redefining.
const (
	NameMinLength = 2
	NameMaxLength = 100
)

// ErrInvalid is the sentinel for invariant violations — wrapped via
// fmt.Errorf("%w: ...", ErrInvalid) at each call site so callers
// branch via errors.Is(err, role.ErrInvalid).
var ErrInvalid = errs.New(errs.KindInvalidInput, "role", "invalid role")

// ErrSystemDefault is returned when a mutation targets a system-
// default role (rename, hierarchy change, delete). System defaults
// are immutable — tenants must clone them to a custom role to mutate.
var ErrSystemDefault = errors.New("role: cannot mutate system-default role")

// ErrDeleted is returned when a mutation targets an already-deleted
// role.
var ErrDeleted = errors.New("role: cannot mutate deleted role")

// ID is the Role primary key (UUIDv7 string form).
type ID string

// IsZero reports whether the ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Role is the aggregate root. Tenant-scoped via `tenantID`.
//
// Invariants:
//   - id, tenantID non-zero.
//   - name trimmed length in [2, 100].
//   - hierarchyLevel in [HierarchyLevelMin, HierarchyLevelMax].
//   - isSuperAdmin set only by trusted seed code (Platform tenant
//     SuperAdmin role) — drives JWT `is_super_user` claim per
//     `multi-tenancy.md` "SuperUser god-mode."
//   - deleted is one-way; UpdateByID rejects mutations after delete.
//
// Hierarchy/parent links live in the [rolehierarchy] aggregate per
// ADR 0058 — they're NOT a property of Role.
type Role struct {
	id              ID
	tenantID        tenant.ID
	name            string
	isSystemDefault bool
	isSuperAdmin    bool
	hierarchyLevel  int
	permissions     []*permission.Permission
	createdAt       time.Time
	deleted         bool
	deletedAt       time.Time
	deletedBy       string

	events []Event
}

// New constructs a brand-new Role. Returns [ErrInvalid] (wrapped) on
// invariant violation.
//
// hierarchyLevel must be in [HierarchyLevelMin, HierarchyLevelMax].
// Pass [HierarchyLevelDefault] for safe middle-of-band placement.
//
// isSuperAdmin: reserved for the seeded SuperAdmin role in the
// Platform tenant. Set true ONLY by trusted seed code per
// `multi-tenancy.md` "SuperUser god-mode." Tenant admins cannot
// promote a custom role to SuperAdmin via HTTP — the field is
// constructor-only, no setter.
//
// `now` is the explicit creation instant per the clock-injection
// refactor — caller threads the same value used for any sibling
// aggregate transitions in the operation.
func New(
	id ID,
	tenantID tenant.ID,
	name string,
	isSystemDefault bool,
	hierarchyLevel int,
	isSuperAdmin bool,
	now time.Time,
) (*Role, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalid)
	}
	trimmed, err := validateName(name)
	if err != nil {
		return nil, err
	}
	if hierarchyLevel < HierarchyLevelMin || hierarchyLevel > HierarchyLevelMax {
		return nil, fmt.Errorf("%w: hierarchyLevel %d not in [%d,%d]",
			ErrInvalid, hierarchyLevel, HierarchyLevelMin, HierarchyLevelMax)
	}

	now = now.UTC()
	r := &Role{
		id:              id,
		tenantID:        tenantID,
		name:            trimmed,
		isSystemDefault: isSystemDefault,
		isSuperAdmin:    isSuperAdmin,
		hierarchyLevel:  hierarchyLevel,
		createdAt:       now,
	}
	r.recordEvent(CreatedEvent{
		RoleID:          id,
		TenantID:        tenantID,
		Name:            trimmed,
		IsSystemDefault: isSystemDefault,
		HierarchyLevel:  hierarchyLevel,
		IsSuperAdmin:    isSuperAdmin,
		At:              now,
	})
	return r, nil
}

// ----- Getters --------------------------------------------------------------

// ID returns the Role's primary key.
func (r *Role) ID() ID { return r.id }

// TenantID returns the FK to [tenant.Tenant] this Role belongs to.
func (r *Role) TenantID() tenant.ID { return r.tenantID }

// Name returns the user-facing display name.
func (r *Role) Name() string { return r.name }

// IsSystemDefault reports whether this Role was seeded by
// [DefaultRoleCatalog] (immutable: refuses Rename / Delete /
// ChangeHierarchyLevel).
func (r *Role) IsSystemDefault() bool { return r.isSystemDefault }

// IsSuperAdmin reports whether this Role drives the SuperUser
// authorization short-circuit per `multi-tenancy.md` "SuperUser
// god-mode". True only on the Platform-tenant SuperAdmin role
// (constructor-only flag; tenant admins cannot promote).
func (r *Role) IsSuperAdmin() bool { return r.isSuperAdmin }

// HierarchyLevel returns the numeric authority position
// (lower = higher authority; bounded [HierarchyLevelMin,
// HierarchyLevelMax]).
func (r *Role) HierarchyLevel() int { return r.hierarchyLevel }

// CreatedAt returns the immutable creation timestamp.
func (r *Role) CreatedAt() time.Time { return r.createdAt }

// IsDeleted reports whether the Role has been soft-deleted.
// Live read paths (GetByID / ListByTenant / GetByIDs) filter
// deleted rows; this getter is for forensic / admin tooling.
func (r *Role) IsDeleted() bool { return r.deleted }

// DeletedAt returns the soft-delete timestamp; zero if live.
func (r *Role) DeletedAt() time.Time { return r.deletedAt }

// DeletedBy returns the user identifier that performed the
// soft-delete; empty if live.
func (r *Role) DeletedBy() string { return r.deletedBy }

// Permissions returns a defensive copy of the role's permission set.
// Callers that mutate the returned slice will not affect the role
// state — mutations go through [Role.GrantPermission] /
// [Role.RevokePermission] / [Role.ReplacePermissions] (Task 9).
func (r *Role) Permissions() []*permission.Permission {
	out := make([]*permission.Permission, len(r.permissions))
	copy(out, r.permissions)
	return out
}

// ----- State transitions ----------------------------------------------------

// Rename changes the Role's display name. System-default roles refuse
// rename per `multi-tenancy.md` doctrine.
//
// Idempotent: renaming to the (trimmed) same name is a no-op (no event).
//
// `now` is the explicit instant for the emitted event's `At`.
func (r *Role) Rename(newName string, now time.Time) error {
	if err := r.ensureMutable(); err != nil {
		return err
	}
	if r.isSystemDefault {
		return fmt.Errorf("%w: %s", ErrSystemDefault, r.name)
	}
	trimmed, err := validateName(newName)
	if err != nil {
		return err
	}
	if trimmed == r.name {
		return nil
	}
	old := r.name
	r.name = trimmed
	r.recordEvent(RenamedEvent{
		RoleID:   r.id,
		TenantID: r.tenantID,
		OldName:  old,
		NewName:  trimmed,
		At:       now.UTC(),
	})
	return nil
}

// ChangeHierarchyLevel updates the numeric authority position. System-
// default roles refuse change. No domain event emitted — hierarchy
// changes are operational concerns, not user-facing audit events.
//
// Idempotent: setting to the current level is a no-op.
func (r *Role) ChangeHierarchyLevel(level int) error {
	if err := r.ensureMutable(); err != nil {
		return err
	}
	if r.isSystemDefault {
		return fmt.Errorf("%w: %s", ErrSystemDefault, r.name)
	}
	if level < HierarchyLevelMin || level > HierarchyLevelMax {
		return fmt.Errorf("%w: hierarchyLevel %d not in [%d,%d]",
			ErrInvalid, level, HierarchyLevelMin, HierarchyLevelMax)
	}
	if level == r.hierarchyLevel {
		return nil
	}
	r.hierarchyLevel = level
	return nil
}

// HasPermission reports whether the role grants `p`. Used by the
// effective-permission resolver (Task 13 / Task 21) when computing a
// Membership's authoritative set. Pointer-equality on interned
// permissions makes this O(N) over a small N (~30 max in catalogue).
func (r *Role) HasPermission(p *permission.Permission) bool {
	if p == nil {
		return false
	}
	return slices.ContainsFunc(r.permissions, p.Equal)
}

// GrantPermission adds `p` to the role's permission set. Idempotent —
// granting an already-granted permission is a no-op (no event).
//
// `now` is the explicit instant for the emitted event's `At`.
func (r *Role) GrantPermission(p *permission.Permission, now time.Time) error {
	if err := r.ensureMutable(); err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("%w: permission required", ErrInvalid)
	}
	if slices.ContainsFunc(r.permissions, p.Equal) {
		return nil
	}
	r.permissions = append(r.permissions, p)
	r.recordEvent(PermissionGrantedEvent{
		RoleID:     r.id,
		TenantID:   r.tenantID,
		Permission: p.Name(),
		At:         now.UTC(),
	})
	return nil
}

// RevokePermission removes `p` from the role's permission set.
// Idempotent — revoking a non-present permission is a no-op (no event).
//
// `now` is the explicit instant for the emitted event's `At`.
func (r *Role) RevokePermission(p *permission.Permission, now time.Time) error {
	if err := r.ensureMutable(); err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("%w: permission required", ErrInvalid)
	}
	idx := slices.IndexFunc(r.permissions, p.Equal)
	if idx < 0 {
		return nil
	}
	r.permissions = slices.Delete(r.permissions, idx, idx+1)
	r.recordEvent(PermissionRevokedEvent{
		RoleID:     r.id,
		TenantID:   r.tenantID,
		Permission: p.Name(),
		At:         now.UTC(),
	})
	return nil
}

// ReplacePermissions sets the permission set to `target`. Emits
// PermissionRevokedEvent for each removal then PermissionGrantedEvent
// for each addition (event order: revoked-before-granted matches the
// natural audit-log narrative). Empty/nil target revokes everything.
//
// Nil entries in `target` are silently dropped (defensive — callers
// shouldn't pass nils, but we don't crash if they do).
//
// `now` is the explicit instant shared by every emitted Revoked/Granted
// event so audit consumers see one timestamp per ReplacePermissions
// operation.
func (r *Role) ReplacePermissions(target []*permission.Permission, now time.Time) error {
	if err := r.ensureMutable(); err != nil {
		return err
	}
	now = now.UTC()
	// Set membership is by permission NAME (value identity), not pointer:
	// a caller may pass freshly-constructed *Permission that aren't the
	// catalogue-interned pointers, and a pointer-keyed set would treat a
	// same-named permission as both "revoke old" and "grant new".
	wantSet := make(map[string]struct{}, len(target))
	for _, p := range target {
		if p != nil {
			wantSet[p.Name()] = struct{}{}
		}
	}
	currentSet := make(map[string]struct{}, len(r.permissions))
	for _, p := range r.permissions {
		currentSet[p.Name()] = struct{}{}
	}
	// Keep current entries still wanted; emit Revoke for the rest.
	kept := make([]*permission.Permission, 0, len(r.permissions))
	for _, p := range r.permissions {
		if _, keep := wantSet[p.Name()]; keep {
			kept = append(kept, p)
			continue
		}
		r.recordEvent(PermissionRevokedEvent{
			RoleID:     r.id,
			TenantID:   r.tenantID,
			Permission: p.Name(),
			At:         now,
		})
	}
	r.permissions = kept
	// Grant additions. Iterate target (not the map) for deterministic
	// event order; skip nils + anything already present.
	for _, p := range target {
		if p == nil {
			continue
		}
		if _, already := currentSet[p.Name()]; already {
			continue
		}
		currentSet[p.Name()] = struct{}{} // guard against duplicate names in target
		r.permissions = append(r.permissions, p)
		r.recordEvent(PermissionGrantedEvent{
			RoleID:     r.id,
			TenantID:   r.tenantID,
			Permission: p.Name(),
			At:         now,
		})
	}
	return nil
}

// Delete soft-deletes the role. System-default roles refuse delete.
// Idempotent — calling Delete on an already-deleted role is a no-op
// (no event).
//
// CALLER INVARIANT: the application service MUST verify NO Membership
// holds an active assignment to this role before calling Delete (or
// must mass-revoke first via the role-assignment subscriber). The
// domain doesn't reach across aggregates — that's an application-tier
// concern per Vernon ch.10.
//
// `now` is the explicit soft-delete instant.
func (r *Role) Delete(deletedBy string, now time.Time) error {
	if r.deleted {
		return nil
	}
	if r.isSystemDefault {
		return fmt.Errorf("%w: %s", ErrSystemDefault, r.name)
	}
	now = now.UTC()
	r.deleted = true
	r.deletedAt = now
	r.deletedBy = deletedBy
	r.recordEvent(DeletedEvent{
		RoleID:    r.id,
		TenantID:  r.tenantID,
		DeletedBy: deletedBy,
		At:        now,
	})
	return nil
}

// ----- Persistence DTO ------------------------------------------------------

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
// Mirror of the database-row shape — sqlc + repository fill this from
// SELECT results, then call UnmarshalFromDB to construct the
// in-memory aggregate.
type Snapshot struct {
	ID              ID
	TenantID        tenant.ID
	Name            string
	IsSystemDefault bool
	IsSuperAdmin    bool
	HierarchyLevel  int
	Permissions     []*permission.Permission
	CreatedAt       time.Time
	IsDeleted       bool
	DeletedAt       time.Time
	DeletedBy       string
}

// UnmarshalFromDB rehydrates a Role from persistence. Repository-only
// path; does NOT re-validate (TDL canon — DB-stored data is already
// invariant-checked at write time, re-checking on every read costs
// without benefit). Does NOT emit domain events — rehydration is not
// a domain transition.
func UnmarshalFromDB(s Snapshot) *Role {
	return &Role{
		id:              s.ID,
		tenantID:        s.TenantID,
		name:            s.Name,
		isSystemDefault: s.IsSystemDefault,
		isSuperAdmin:    s.IsSuperAdmin,
		hierarchyLevel:  s.HierarchyLevel,
		permissions:     append([]*permission.Permission(nil), s.Permissions...),
		createdAt:       s.CreatedAt,
		deleted:         s.IsDeleted,
		deletedAt:       s.DeletedAt,
		deletedBy:       s.DeletedBy,
	}
}

// ensureMutable rejects mutations on a deleted role. Internal helper
// for the state-transition methods (Rename / ChangeHierarchyLevel /
// GrantPermission / etc.).
func (r *Role) ensureMutable() error {
	if r.deleted {
		return fmt.Errorf("%w: %s", ErrDeleted, r.name)
	}
	return nil
}

// validateName trims + bounds-checks the role name. Shared by [New]
// and [Role.Rename] — single source of truth for the [NameMinLength,
// NameMaxLength] invariant per `coding-standards.md` "No magic
// strings — production AND tests" + Effective Go DRY.
func validateName(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: name required", ErrInvalid)
	}
	if len(trimmed) < NameMinLength || len(trimmed) > NameMaxLength {
		return "", fmt.Errorf("%w: name length %d not in [%d,%d]",
			ErrInvalid, len(trimmed), NameMinLength, NameMaxLength)
	}
	return trimmed, nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains recorded domain events. Repository calls this
// after a successful persist, then writes each event into the outbox
// in the same transaction (TDL UpdateFn pattern per ADR 0004 + 0008).
func (r *Role) PullEvents() []Event {
	if len(r.events) == 0 {
		return nil
	}
	out := r.events
	r.events = nil
	return out
}

func (r *Role) recordEvent(e Event) {
	r.events = append(r.events, e)
}
