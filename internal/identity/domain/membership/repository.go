package membership

import (
	"context"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by Repository read methods when no Membership matches.
var ErrNotFound = errs.New(errs.KindNotFound, "membership", "membership not found")

// ErrAlreadyActive is returned when a caller attempts to create or reactivate
// a Membership for a Person that already has an Active Membership elsewhere.
//
// The DB partial unique index `WHERE status='Active' AND NOT is_deleted` is
// the authoritative enforcement; this typed error is what the adapter
// surfaces from the SQLSTATE 23505 constraint violation.
var ErrAlreadyActive = errs.New(errs.KindConflict, "membership", "person already has active membership")

// Repository persists Membership aggregates.
//
// Membership IS tenant-scoped — RLS policies on `identity.tenant_memberships`
// (per ADR 0006) restrict reads to the current tenant's membership rows.
// Cross-tenant resolution for the login path lives in
// [command.AuthRouter] (a separate concern that JOINs persons +
// tenant_memberships in one indexed roundtrip — current canon over
// the legacy denormalised auth_routing-table approach).
// GetActiveForPerson on this repo serves non-login cross-tenant
// callers (subscribers, audit, platform-operator UIs).
type Repository interface {
	// Add persists a brand-new Membership from [New]. Returns
	// [ErrAlreadyActive] if the PersonID already has an Active Membership.
	// The aggregate carries TenantID — no explicit parameter needed.
	Add(ctx context.Context, m *Membership) error

	// UpdateByID loads, mutates via updateFn, persists, drains events.
	// Per ADR 0004 + TDL Sep 2024 UpdateFn pattern. Per ADR 0062 the
	// tenantID is an explicit parameter — the SQL adapter binds it
	// onto the tx GUC for RLS; the fake filters by it for parity.
	// Returns [ErrNotFound] if the Membership doesn't exist OR exists
	// in a different tenant scope (RLS-hidden — same observable
	// behaviour as truly missing).
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Membership) (bool, error)) error

	// GetByID returns the Membership or [ErrNotFound]. Per ADR 0062
	// tenantID is an explicit parameter — the SQL adapter binds it
	// onto the tx GUC for RLS; the fake filters by it for parity.
	// Returns [ErrNotFound] when the row exists in a different tenant.
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Membership, error)

	// GetActiveForPerson returns the (single) Active Membership for a
	// Person across all tenants. Used by NON-login cross-tenant callers
	// (cascade subscribers, audit, platform-operator UIs). The login
	// flow uses [command.AuthRouter] instead — single JOIN persons +
	// tenant_memberships, one fewer roundtrip.
	//
	// Backed by the partial-unique index `uq_memberships_person_active`
	// (`person_id WHERE status='active'`) — single-row lookup, runs
	// under TxScopePlatform to bypass RLS. Returns [ErrNotFound] if
	// the Person has no Active Membership.
	GetActiveForPerson(ctx context.Context, personID person.ID) (*Membership, error)

	// ListForTenant returns all Memberships under the current tenant scope.
	// Used by tenant admin UIs ("manage users").
	ListForTenant(ctx context.Context, tenantID tenant.ID) ([]*Membership, error)

	// ListForTenantPage returns the next keyset-paginated slice of ACTIVE
	// Memberships under the current tenant scope, per ADR 0038.
	//
	// Cursor semantics: (beforeJoinedAt, beforeID) is the previous-page
	// boundary. First page supplies sentinels — a future timestamp +
	// the all-ones UUID — so the tuple comparison admits every row.
	//
	// limit MUST be the caller's desired page_size + 1 (the "peek one
	// extra" trick from ADR 0038); the caller drops the extra row when
	// present + uses it to set next_cursor.
	//
	// Returns only Memberships with status='active' — matches the
	// partial composite index idx_memberships_tenant_active_joined.
	// Inactive listing path is a future ?status=inactive query.
	//
	// Primitive types here (time.Time + string + int) keep the domain
	// layer free of pagination-package coupling per ADR 0002. The
	// application-layer query handler is responsible for cursor
	// encode/decode + sentinel construction.
	ListForTenantPage(ctx context.Context, tenantID tenant.ID, beforeJoinedAt time.Time, beforeID string, limit int) ([]*Membership, error)

	// ListAllForPerson returns every Membership (Active + Inactive) the
	// supplied Person holds across ALL tenants. Cross-tenant read —
	// platform-only path. Used by:
	//   - Platform operator "show all of this user's tenants" UI
	//   - Person-level cascades (anonymise, global suspend) where the
	//     handler needs to enumerate the affected memberships before
	//     dispatching per-tenant fanout events.
	//
	// Implementation: backed by the cross-tenant ListMembershipsForPerson
	// query which Postgres permits because the partial index on
	// (person_id) is non-RLS-filtered (per database.md "Single-Active-
	// Membership constraint"). Returns an empty slice (not ErrNotFound)
	// when the Person has no Memberships.
	ListAllForPerson(ctx context.Context, personID person.ID) ([]*Membership, error)

	// HasActiveSuperAdmin reports whether the supplied tenant has any
	// active Membership holding a role flagged is_super_admin=true.
	//
	// Backstop for the platform-tenant deletion guard: tenants holding
	// any SuperAdmin role-holder cannot be Suspended, MarkedForDeletion,
	// or HardDeleted via the standard lifecycle commands — the platform
	// would lose its god-mode operator surface if accidentally deleted.
	//
	// Cross-tenant query — runs under TxScopePlatform. Uses the partial
	// index idx_roles_super_admin (O(1) where-clause) so the call is
	// cheap enough to execute on every lifecycle command.
	HasActiveSuperAdmin(ctx context.Context, tenantID tenant.ID) (bool, error)
}
