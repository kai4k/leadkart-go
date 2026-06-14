// Package adapters holds the Notifications module's outbound adapters —
// the pgx/sqlc-backed repository. Tenant-scoped via TxScopeTenant; recipient
// isolation is applied in the query, not RLS.
package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/adapters/db"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

// DedupWindow mirrors the time-windowed dedup probe in the Add path: a sourced
// notification to the same recipient within this window of an existing one is
// suppressed (at-least-once replay safety). Matches notificationtest.DedupWindow.
const DedupWindow = 5 * time.Minute

// maxCursorTime is the sentinel upper bound for the keyset cursor's first page
// (created_at, id) < (max, max).
func maxCursorTime() time.Time { return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC) }

// maxCursorUUID is the all-FF first-page tiebreak cursor (greater than every
// realistic UUIDv7). Byte-array literal — no MustParse in the request path.
func maxCursorUUID() uuid.UUID {
	return uuid.UUID{
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	}
}

// NotificationRepository is the pgx/sqlc-backed [notification.Repository].
type NotificationRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewNotificationRepository constructs a NotificationRepository.
func NewNotificationRepository(pool *pgxpool.Pool, tx *pg.Transactor) *NotificationRepository {
	return &NotificationRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Compile-time interface conformance.
var _ notification.Repository = (*NotificationRepository)(nil)

// Add satisfies [notification.Repository]. Sourced notifications use the
// time-windowed dedup insert (→ ErrDuplicateInDedupWindow on suppression);
// manual ones use a plain insert. Joins the ambient UoW tx when present.
func (r *NotificationRepository) Add(ctx context.Context, n *notification.Notification) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, n)
	}
	return r.tx.WithinTxPgxTenant(ctx, n.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, n)
	})
}

func (r *NotificationRepository) addOnTx(ctx context.Context, tx pgx.Tx, n *notification.Notification) error {
	q := r.q.WithTx(tx)
	id, err := uuid.Parse(n.ID().String())
	if err != nil {
		return fmt.Errorf("notification repo: parse id: %w", err)
	}
	tid, err := uuid.Parse(n.TenantID().String())
	if err != nil {
		return fmt.Errorf("notification repo: parse tenant id: %w", err)
	}
	rid, err := uuid.Parse(n.RecipientMembershipID().String())
	if err != nil {
		return fmt.Errorf("notification repo: parse recipient id: %w", err)
	}
	createdBy := membershipToPgUUID(n.CreatedByMembershipID())

	if n.SourceEntityID() != "" {
		cutoff := n.CreatedAt().Add(-DedupWindow)
		rows, derr := q.InsertNotificationDeduped(ctx, db.InsertNotificationDedupedParams{
			ID:                    pgconv.PgUUID(id),
			TenantID:              pgconv.PgUUID(tid),
			RecipientMembershipID: pgconv.PgUUID(rid),
			Category:              n.Category().String(),
			Title:                 n.Title(),
			Body:                  n.Body(),
			State:                 n.State().String(),
			SourceModule:          n.SourceModule(),
			SourceEntityType:      n.SourceEntityType(),
			SourceEntityID:        n.SourceEntityID(),
			DeepLink:              n.DeepLink(),
			CreatedByMembershipID: createdBy,
			CreatedAt:             pgconv.PgRequiredTimestamp(n.CreatedAt()),
			DedupCutoff:           pgconv.PgRequiredTimestamp(cutoff),
		})
		if derr != nil {
			return fmt.Errorf("notification repo: deduped insert: %w", derr)
		}
		if rows == 0 {
			return notification.ErrDuplicateInDedupWindow
		}
		_ = n.PullEvents() // no integration events in v1 (real-time push is a follow-up)
		return nil
	}

	if ierr := q.InsertNotification(ctx, db.InsertNotificationParams{
		ID:                    pgconv.PgUUID(id),
		TenantID:              pgconv.PgUUID(tid),
		RecipientMembershipID: pgconv.PgUUID(rid),
		Category:              n.Category().String(),
		Title:                 n.Title(),
		Body:                  n.Body(),
		State:                 n.State().String(),
		SourceModule:          n.SourceModule(),
		SourceEntityType:      n.SourceEntityType(),
		SourceEntityID:        n.SourceEntityID(),
		DeepLink:              n.DeepLink(),
		CreatedByMembershipID: createdBy,
		CreatedAt:             pgconv.PgRequiredTimestamp(n.CreatedAt()),
	}); ierr != nil {
		return fmt.Errorf("notification repo: insert: %w", ierr)
	}
	_ = n.PullEvents()
	return nil
}

// GetByID satisfies [notification.Repository]. GUC bound from tenantID.
func (r *NotificationRepository) GetByID(ctx context.Context, tenantID tenant.ID, id notification.ID) (*notification.Notification, error) {
	var out *notification.Notification
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := loadNotification(ctx, r.q.WithTx(tx), tenantID, id)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateByID satisfies [notification.Repository]. Loads, mutates, persists the
// state transition. Joins the ambient UoW tx when present.
func (r *NotificationRepository) UpdateByID(ctx context.Context, tenantID tenant.ID, id notification.ID, mutator func(*notification.Notification) (bool, error)) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, tenantID, id, mutator)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, tenantID, id, mutator)
	})
}

func (r *NotificationRepository) updateOnTx(ctx context.Context, tx pgx.Tx, tenantID tenant.ID, id notification.ID, mutator func(*notification.Notification) (bool, error)) error {
	q := r.q.WithTx(tx)
	n, err := loadNotification(ctx, q, tenantID, id)
	if err != nil {
		return err
	}
	commit, err := mutator(n)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	nid, err := uuid.Parse(n.ID().String())
	if err != nil {
		return fmt.Errorf("notification repo: parse id: %w", err)
	}
	if uerr := q.UpdateNotificationState(ctx, db.UpdateNotificationStateParams{
		ID:          pgconv.PgUUID(nid),
		State:       n.State().String(),
		ReadAt:      pgconv.PgTimestampPtr(n.ReadAt()),
		DismissedAt: pgconv.PgTimestampPtr(n.DismissedAt()),
	}); uerr != nil {
		return fmt.Errorf("notification repo: update state: %w", uerr)
	}
	_ = n.PullEvents()
	return nil
}

// ListPageForRecipient satisfies [notification.Repository]. Keyset on
// (created_at DESC, id DESC). GUC bound from tenantID.
func (r *NotificationRepository) ListPageForRecipient(ctx context.Context, tenantID tenant.ID, recipient membership.ID, filter notification.ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*notification.Notification], error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return pagination.Page[*notification.Notification]{}, fmt.Errorf("notification repo: parse tenant id: %w", err)
	}
	rid, err := uuid.Parse(recipient.String())
	if err != nil {
		return pagination.Page[*notification.Notification]{}, fmt.Errorf("notification repo: parse recipient id: %w", err)
	}

	cursorCreatedAt := pgconv.PgRequiredTimestamp(maxCursorTime())
	cursorID := pgconv.PgUUID(maxCursorUUID())
	if cursor.ID != "" {
		cursorCreatedAt = pgconv.PgRequiredTimestamp(cursor.SortValue)
		cid, perr := uuid.Parse(cursor.ID)
		if perr != nil {
			return pagination.Page[*notification.Notification]{}, fmt.Errorf("notification repo: parse cursor id: %w", perr)
		}
		cursorID = pgconv.PgUUID(cid)
	}

	var out []*notification.Notification
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, qerr := r.q.WithTx(tx).ListNotificationsForRecipientPage(ctx, db.ListNotificationsForRecipientPageParams{
			TenantID:              pgconv.PgUUID(tid),
			RecipientMembershipID: pgconv.PgUUID(rid),
			StateFilter:           filter.State.String(),
			CategoryFilter:        filter.Category.String(),
			CursorCreatedAt:       cursorCreatedAt,
			CursorID:              cursorID,
			//nolint:gosec // pageSize clamped to MaxPageSize by the caller
			Limit: int32(pageSize + 1),
		})
		if qerr != nil {
			return fmt.Errorf("notification repo: list page: %w", qerr)
		}
		for _, row := range rows {
			out = append(out, rowToNotification(row))
		}
		return nil
	})
	if err != nil {
		return pagination.Page[*notification.Notification]{}, err
	}
	page := pagination.BuildPage(out, pageSize, func(n *notification.Notification) pagination.Cursor {
		return pagination.Cursor{SortValue: n.CreatedAt(), ID: n.ID().String()}
	})
	if page.Items == nil {
		page.Items = []*notification.Notification{}
	}
	return page, nil
}

// CountUnreadForRecipient satisfies [notification.Repository]. GUC bound from tenantID.
func (r *NotificationRepository) CountUnreadForRecipient(ctx context.Context, tenantID tenant.ID, recipient membership.ID) (int64, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return 0, fmt.Errorf("notification repo: parse tenant id: %w", err)
	}
	rid, err := uuid.Parse(recipient.String())
	if err != nil {
		return 0, fmt.Errorf("notification repo: parse recipient id: %w", err)
	}
	var n int64
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		got, qerr := r.q.WithTx(tx).CountUnreadForRecipient(ctx, db.CountUnreadForRecipientParams{
			TenantID:              pgconv.PgUUID(tid),
			RecipientMembershipID: pgconv.PgUUID(rid),
		})
		if qerr != nil {
			return fmt.Errorf("notification repo: count unread: %w", qerr)
		}
		n = got
		return nil
	})
	return n, err
}

// UpdateAllUnreadForRecipient satisfies [notification.Repository]. Bulk-flips
// unread → read. readAt is unix-nano (the canonical wire form for the bulk
// path); returns rows affected. GUC bound from tenantID.
func (r *NotificationRepository) UpdateAllUnreadForRecipient(ctx context.Context, tenantID tenant.ID, recipient membership.ID, readAt int64) (int64, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return 0, fmt.Errorf("notification repo: parse tenant id: %w", err)
	}
	rid, err := uuid.Parse(recipient.String())
	if err != nil {
		return 0, fmt.Errorf("notification repo: parse recipient id: %w", err)
	}
	run := func(ctx context.Context, tx pgx.Tx) (int64, error) {
		return r.q.WithTx(tx).MarkAllUnreadReadForRecipient(ctx, db.MarkAllUnreadReadForRecipientParams{
			TenantID:              pgconv.PgUUID(tid),
			RecipientMembershipID: pgconv.PgUUID(rid),
			ReadAt:                pgconv.PgRequiredTimestamp(time.Unix(0, readAt).UTC()),
		})
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	var affected int64
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		got, qerr := run(ctx, tx)
		if qerr != nil {
			return fmt.Errorf("notification repo: mark all read: %w", qerr)
		}
		affected = got
		return nil
	})
	return affected, err
}

// ----- Helpers ---------------------------------------------------------------

func loadNotification(ctx context.Context, q *db.Queries, tenantID tenant.ID, id notification.ID) (*notification.Notification, error) {
	nid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("notification repo: parse id: %w", err)
	}
	row, err := q.GetNotificationByID(ctx, pgconv.PgUUID(nid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notification.ErrNotFound
		}
		return nil, fmt.Errorf("notification repo: get by id: %w", err)
	}
	n := rowToNotification(row)
	// RLS hides cross-tenant rows; the explicit check guards the join-ambient-tx
	// path where the GUC may differ from the requested tenant.
	if n.TenantID() != tenantID {
		return nil, notification.ErrNotFound
	}
	return n, nil
}

func rowToNotification(row db.NotificationsNotification) *notification.Notification {
	createdBy := membership.ID("")
	if row.CreatedByMembershipID.Valid {
		createdBy = membership.ID(pgconv.UUIDFromPg(row.CreatedByMembershipID).String())
	}
	return notification.UnmarshalFromDB(notification.Snapshot{
		ID:                    notification.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:              tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		RecipientMembershipID: membership.ID(pgconv.UUIDFromPg(row.RecipientMembershipID).String()),
		Category:              notification.Category(row.Category),
		Title:                 row.Title,
		Body:                  row.Body,
		State:                 notification.State(row.State),
		SourceModule:          row.SourceModule,
		SourceEntityType:      row.SourceEntityType,
		SourceEntityID:        row.SourceEntityID,
		DeepLink:              row.DeepLink,
		CreatedByMembershipID: createdBy,
		CreatedAt:             pgconv.TimeFromPg(row.CreatedAt),
		ReadAt:                pgconv.TimePtrFromPg(row.ReadAt),
		DismissedAt:           pgconv.TimePtrFromPg(row.DismissedAt),
	})
}

// membershipToPgUUID maps an optional membership id to a nullable pg uuid —
// zero/empty (system/cron source) → NULL.
func membershipToPgUUID(m membership.ID) pgtype.UUID {
	if m.String() == "" {
		return pgconv.PgUUIDOrNull(uuid.Nil)
	}
	parsed, err := uuid.Parse(m.String())
	if err != nil {
		return pgconv.PgUUIDOrNull(uuid.Nil)
	}
	return pgconv.PgUUIDOrNull(parsed)
}
