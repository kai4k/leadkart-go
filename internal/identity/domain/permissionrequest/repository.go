package permissionrequest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
)

// Repository persists [Request] aggregates. Per Cheney "accept
// interfaces, return structs" the contract lives in the domain layer;
// the concrete pgx/sqlc adapter lives in internal/identity/adapters/.
//
// Tenant scoping: identity.permission_requests is RLS+FORCE — every
// read/write runs under [pg.TxScopeTenant] so app.tenant_id GUC binds
// the connection before queries fire. Cross-tenant audit / support
// tooling MUST run under platform scope.
type Repository interface {
	// Add persists a brand-new Pending Request. Returns
	// [ErrPendingRequestExists] if the partial unique index
	// uq_permission_requests_pending refuses the INSERT (another
	// Pending row for the same (requester, permission) tuple already
	// exists). Drains domain events into the outbox same-tx per
	// ADR 0008.
	Add(ctx context.Context, r *Request) error

	// UpdateByID loads, mutates via updateFn, persists, drains events.
	// Per TDL Sep 2024 UpdateFn pattern (ADR 0004). Returns
	// [ErrNotFound] if the row doesn't exist or RLS hides it.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Request) (bool, error)) error

	// GetByID returns the Request or [ErrNotFound]. RLS-scoped read.
	GetByID(ctx context.Context, id ID) (*Request, error)

	// GetPendingForMembership returns every Pending request the
	// supplied Membership has open. Used by the application service to
	// pre-validate the at-most-one-pending invariant before mint AND
	// by the requester's UI ("show my pending elevations").
	GetPendingForMembership(ctx context.Context, m membership.ID) ([]*Request, error)

	// ListPendingApprovableBy returns the keyset-paginated queue of
	// Pending requests where approver_membership_id matches the
	// supplied Membership ID. Per ADR 0038 cursor semantics; first
	// page passes the zero pagination.Cursor + the adapter applies its
	// own sentinel internally.
	ListPendingApprovableBy(
		ctx context.Context,
		approver membership.ID,
		pageSize int,
		cursor pagination.Cursor,
	) (pagination.Page[*Request], error)

	// ListByRequester returns the keyset-paginated history of every
	// state for the supplied Requester Membership. Used by the
	// requester's "my requests" UI.
	ListByRequester(
		ctx context.Context,
		requester membership.ID,
		pageSize int,
		cursor pagination.Cursor,
	) (pagination.Page[*Request], error)
}
