package notification

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound — no row for the (tenantID, id) pair. Map to HTTP 404.
var ErrNotFound = errors.New("notification: not found")

// ErrDuplicateInDedupWindow — partial unique index caught a fresh
// Add inside the 5-minute dedup window for the same
// (recipient, source_entity_type, source_entity_id, category). The
// subscriber treats this as success (the previous-emitted notification
// is the one the recipient already has).
var ErrDuplicateInDedupWindow = errors.New("notification: duplicate within dedup window")

// ListFilter narrows a [Repository.ListPage] query. Empty fields are
// no-filter; State filters on a single state when set.
type ListFilter struct {
	State    State
	Category Category
}

// Repository persists [Notification] aggregates.
type Repository interface {
	// Add persists a brand-new Notification. Returns
	// [ErrDuplicateInDedupWindow] when the partial unique index
	// catches a duplicate from the producer-side replay.
	Add(ctx context.Context, n *Notification) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Notification, error)

	// UpdateByID runs mutator inside a UoW tx; (true, nil) persists +
	// drains events; (false, nil) is a no-op; non-nil err rolls back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID,
		mutator func(*Notification) (bool, error)) error

	// ListPageForRecipient returns the recipient's inbox, ordered
	// newest first via keyset pagination (per ADR 0038).
	ListPageForRecipient(
		ctx context.Context,
		tenantID tenant.ID,
		recipient membership.ID,
		filter ListFilter,
		cursor pagination.Cursor,
		pageSize int,
	) (pagination.Page[*Notification], error)

	// CountUnreadForRecipient returns the number of unread notifications
	// for the recipient — drives the badge in the UI. Persistence-verb
	// shape (Count) per TDL canon §3.
	CountUnreadForRecipient(ctx context.Context, tenantID tenant.ID, recipient membership.ID) (int64, error)

	// UpdateAllUnreadForRecipient bulk-flips every unread notification
	// for the recipient to read with the supplied timestamp. Returns
	// the number of rows affected. Persistence-verb shape (Update) per
	// TDL canon §3 — the business-verb "MarkRead" lives on the
	// aggregate; this is the SQL-side bulk equivalent used by the
	// inbox `read-all` HTTP handler.
	UpdateAllUnreadForRecipient(
		ctx context.Context, tenantID tenant.ID, recipient membership.ID, readAt int64,
	) (int64, error)
}
