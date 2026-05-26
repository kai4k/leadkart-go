package permissionrequest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Repository persists [Request] aggregates. Per Cheney "accept
// interfaces, return structs" the contract lives in the domain layer;
// the concrete pgx/sqlc adapter lives in internal/identity/adapters/.
//
// Tenant scoping (ADR 0062 — TDL canon): identity.permission_requests
// is RLS+FORCE. Every method that takes an ID without an aggregate
// ALSO takes an EXPLICIT tenantID parameter — the adapter binds the
// GUC from the parameter at tx-begin (NOT from ctx-tenancy.WithID,
// which Khorikov §11 + Cheney mark as a hidden input). RLS remains
// the security backstop; the explicit param is the API surface
// contract. Cross-tenant audit / support tooling MUST run under
// platform scope.
type Repository interface {
	// Add persists a brand-new Pending Request. The aggregate already
	// carries its TenantID — no separate param needed. Returns
	// [ErrPendingRequestExists] if the partial unique index
	// uq_permission_requests_pending refuses the INSERT (another
	// Pending row for the same (requester, permission) tuple already
	// exists). Drains domain events into the outbox same-tx per
	// ADR 0008.
	Add(ctx context.Context, r *Request) error

	// UpdateByID loads (scoped to tenantID), mutates via updateFn,
	// persists, drains events. Per TDL Sep 2024 UpdateFn pattern
	// (ADR 0004). Returns [ErrNotFound] if the row doesn't exist or
	// RLS hides it.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Request) (bool, error)) error

	// GetByID returns the Request scoped to tenantID, or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Request, error)

	// GetPendingForMembership returns every Pending request the
	// supplied Membership has open in the supplied tenant. Used by
	// the application service to pre-validate the at-most-one-pending
	// invariant before mint AND by the requester's UI ("show my
	// pending elevations").
	GetPendingForMembership(ctx context.Context, tenantID tenant.ID, m membership.ID) ([]*Request, error)

	// ListPendingApprovableBy returns the keyset-paginated queue of
	// Pending requests where approver_membership_id matches the
	// supplied Membership ID, scoped to tenantID. Per ADR 0038 cursor
	// semantics; first page passes the zero pagination.Cursor + the
	// adapter applies its own sentinel internally.
	ListPendingApprovableBy(
		ctx context.Context,
		tenantID tenant.ID,
		approver membership.ID,
		pageSize int,
		cursor pagination.Cursor,
	) (pagination.Page[*Request], error)

	// ListByRequester returns the keyset-paginated history of every
	// state for the supplied Requester Membership, scoped to tenantID.
	// Used by the requester's "my requests" UI.
	ListByRequester(
		ctx context.Context,
		tenantID tenant.ID,
		requester membership.ID,
		pageSize int,
		cursor pagination.Cursor,
	) (pagination.Page[*Request], error)
}
