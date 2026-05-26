package rolehierarchy

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Repository persists [Edge] aggregates. Per Cheney "accept
// interfaces, return structs" the contract lives in the domain
// layer; the concrete pgx/sqlc adapter lives in
// internal/identity/adapters/.
//
// Tenant scoping (ADR 0062 — TDL canon): identity.role_hierarchy_edges
// is RLS+FORCE per ADR 0058. Every method that takes an ID without an
// aggregate ALSO takes an EXPLICIT tenantID parameter — the adapter
// binds the GUC from the parameter at tx-begin (NOT from ctx-
// tenancy.WithID, which Khorikov §11 + Cheney mark as a hidden input).
// RLS remains the security backstop; the explicit param is the API
// surface contract. Platform-bypass available via [pg.TxScopePlatform]
// for cross-tenant audit / support tooling.
//
// All methods MUST be safe for concurrent use by multiple goroutines.
type Repository interface {
	// Add persists a brand-new active Edge. The aggregate already
	// carries its TenantID — no separate param needed. Translates
	// DB-level invariant breaches into typed domain sentinels:
	//   - SQLSTATE 23505 on uq_role_hierarchy_active_edge_per_child →
	//     [ErrEdgeAlreadyExists] (child already has an active parent).
	//   - SQLSTATE 23503 on fk_edges_*_same_tenant → [ErrCrossTenant]
	//     (child + parent belong to different tenants — composite FK
	//     fires; replaces the Wave 9.1d SECURITY DEFINER trigger).
	//   - SQLSTATE 23514 from edge_check_cycle trigger →
	//     [ErrCycle] (multi-hop loop closed).
	//   - SQLSTATE 23514 from chk_edge_no_self_loop CHECK →
	//     [ErrSelfReference] (belt-and-suspenders alongside the
	//     domain New guard).
	//
	// Drains domain events into the outbox same-tx per ADR 0008.
	Add(ctx context.Context, e *Edge) error

	// GetActiveByChild returns the SINGLE active edge for a child,
	// or [ErrEdgeNotFound] if the child has no parent. Scoped to
	// tenantID (GUC bound from the explicit param per ADR 0062).
	GetActiveByChild(ctx context.Context, tenantID tenant.ID, childRoleID role.ID) (*Edge, error)

	// UpdateByID loads the edge (scoped to tenantID), runs updateFn
	// (e.g. Remove), persists + drains events. TDL Sep 2024 UpdateFn
	// pattern (ADR 0004). Returns [ErrEdgeNotFound] if the row doesn't
	// exist or RLS hides it.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Edge) (bool, error)) error

	// GetAncestorsByChild walks the chain upward from `childRoleID`,
	// returning each ancestor edge in depth order (child's parent
	// first → grandparent → … root). The child itself is NOT
	// included; empty result = child is a root (no parent edge).
	// Only ACTIVE edges are walked. Scoped to tenantID.
	GetAncestorsByChild(ctx context.Context, tenantID tenant.ID, childRoleID role.ID) ([]*Edge, error)

	// ListActiveByParent returns every direct child of `parentRoleID`.
	// Used by approval-workflow reporting + future "show this
	// manager's direct reports" admin UI. Scoped to tenantID.
	ListActiveByParent(ctx context.Context, tenantID tenant.ID, parentRoleID role.ID) ([]*Edge, error)
}
