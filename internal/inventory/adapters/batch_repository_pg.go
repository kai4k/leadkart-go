package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// BatchRepository is the pgx/sqlc-backed implementation of
// [batch.Repository]. Tenant-scoped.
//
// Concurrency: UpdateByID acquires a pessimistic row-level lock
// (SELECT ... FOR UPDATE) before the in-memory aggregate load.
// Concurrent transactions block at lock acquisition until the holding
// tx commits or rolls back — Postgres serializes them at the DB layer
// so the application handler does NOT need an optimistic-retry loop.
//
// Canon: Postgres docs §13.3.2 (Explicit Locking) + Stripe ledger
// pattern + DDIA Ch.7 — pessimistic locking is the right primitive
// for hot-row counters (stock_on_hand here, balance there) where
// optimistic-retry under high contention thrashes without making
// forward progress. The `version` column is retained as a defense-
// in-depth + audit signal; the UPDATE's `version = $expected`
// predicate is now never expected to fail because the lock makes it
// impossible for two writers to share the same expected version.
//
// The lock is held only for the duration of the surrounding
// pg.UnitOfWork tx — typically sub-millisecond for the
// LogStockMovement path. If a writer crashes mid-tx, Postgres
// releases the lock on session disconnect (no operator action needed).
type BatchRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewBatchRepository wires the repository.
func NewBatchRepository(pool *pgxpool.Pool, tx *pg.Transactor) *BatchRepository {
	return &BatchRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [batch.Repository]. Joins surrounding UoW tx.
func (r *BatchRepository) Add(ctx context.Context, b *batch.Batch) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, b)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, b)
	})
}

func (r *BatchRepository) addOnTx(ctx context.Context, tx pgx.Tx, b *batch.Batch) error {
	q := r.q.WithTx(tx)
	if err := insertBatchRow(ctx, q, b); err != nil {
		return err
	}
	return drainBatchEvents(ctx, tx, b)
}

// UpdateByID satisfies [batch.Repository]. Acquires a pessimistic
// row-level lock (SELECT ... FOR UPDATE) before loading the aggregate
// so concurrent updaters serialize at the DB layer; see [BatchRepository]
// doc for the rationale.
func (r *BatchRepository) UpdateByID(ctx context.Context, id batch.ID, updateFn func(*batch.Batch) (bool, error)) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

// lockBatchRowForUpdate acquires a pessimistic row-level lock on the
// batches row identified by id, within the supplied tx. Concurrent
// callers block here until this tx commits or rolls back. Returns
// batch.ErrNotFound if the row is missing or soft-deleted (same
// semantics as the subsequent loadBatch read).
//
// The lock is released automatically by Postgres on tx commit/rollback
// or connection drop; no application-level release is required.
func (r *BatchRepository) lockBatchRowForUpdate(ctx context.Context, tx pgx.Tx, id batch.ID) error {
	bid, err := uuid.Parse(id.String())
	if err != nil {
		return fmt.Errorf("batch repo: parse id %q: %w", id, err)
	}
	var locked uuid.UUID
	scanErr := tx.QueryRow(ctx,
		`SELECT id FROM inventory.batches WHERE id = $1 AND NOT is_deleted FOR UPDATE`,
		bid,
	).Scan(&locked)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return batch.ErrNotFound
		}
		return fmt.Errorf("batch repo: lock for update: %w", scanErr)
	}
	return nil
}

func (r *BatchRepository) updateOnTx(ctx context.Context, tx pgx.Tx, id batch.ID, updateFn func(*batch.Batch) (bool, error)) error {
	// Acquire pessimistic row lock first — concurrent updaters block
	// here until our tx commits, so the optimistic-version check below
	// can never fail in production. Lock-then-read is the standard
	// SELECT-FOR-UPDATE-with-snapshot pattern (Postgres §13.3.2).
	if err := r.lockBatchRowForUpdate(ctx, tx, id); err != nil {
		return err
	}
	q := r.q.WithTx(tx)
	b, err := loadBatch(ctx, q, id)
	if err != nil {
		return err
	}
	expectedVersion := b.Version()
	shouldPersist, err := updateFn(b)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	// Persist with WHERE version = $expected as defense-in-depth. The
	// row lock above already prevents concurrent writers from sharing
	// our expected version — rowsAffected==0 here would signal a
	// programmer error (e.g. an external SQL bypass) rather than a
	// real race. Treat it as ErrConcurrencyConflict for clear surfacing.
	rowsAffected, err := persistBatchState(ctx, q, b, expectedVersion)
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return batch.ErrConcurrencyConflict
	}
	return drainBatchEvents(ctx, tx, b)
}

// GetByID satisfies [batch.Repository].
func (r *BatchRepository) GetByID(ctx context.Context, id batch.ID) (*batch.Batch, error) {
	var out *batch.Batch
	err := r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := loadBatch(ctx, q, id)
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

// ListByProductPage satisfies [batch.Repository]. Keyset on
// (expiry_date DESC, id DESC) per migration 20260603000001 index.
func (r *BatchRepository) ListByProductPage(ctx context.Context, productID product.ID, filter batch.ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*batch.Batch], error) {
	pid, err := uuid.Parse(productID.String())
	if err != nil {
		return pagination.Page[*batch.Batch]{}, fmt.Errorf("batch repo: parse product id: %w", err)
	}

	cursorExpiry := pgDate(maxCursorTime())
	cursorID := pgUUID(maxCursorUUID())
	if cursor.ID != "" {
		cursorExpiry = pgDate(cursor.SortValue)
		uid, perr := uuid.Parse(cursor.ID)
		if perr != nil {
			return pagination.Page[*batch.Batch]{}, fmt.Errorf("batch repo: parse cursor id: %w", perr)
		}
		cursorID = pgUUID(uid)
	}

	var out []*batch.Batch
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListBatchesByProductPage(ctx, db.ListBatchesByProductPageParams{
			ProductID:        pgUUID(pid),
			CursorExpiryDate: cursorExpiry,
			CursorID:         cursorID,
			IncludeExpired:   filter.IncludeExpired,
			//nolint:gosec // pageSize clamped to MaxPageSize (200)
			Limit: int32(pageSize + 1),
		})
		if err != nil {
			return fmt.Errorf("batch repo: list page: %w", err)
		}
		for _, row := range rows {
			b, perr := rowToBatch(row)
			if perr != nil {
				return perr
			}
			out = append(out, b)
		}
		return nil
	})
	if err != nil {
		return pagination.Page[*batch.Batch]{}, err
	}
	page := pagination.BuildPage(out, pageSize, func(b *batch.Batch) pagination.Cursor {
		return pagination.Cursor{SortValue: b.ExpiryDate(), ID: b.ID().String()}
	})
	if page.Items == nil {
		page.Items = []*batch.Batch{}
	}
	return page, nil
}

// AnyLiveWithStockForProduct satisfies [batch.Repository].
func (r *BatchRepository) AnyLiveWithStockForProduct(ctx context.Context, productID product.ID) (bool, error) {
	pid, err := uuid.Parse(productID.String())
	if err != nil {
		return false, fmt.Errorf("batch repo: parse product id: %w", err)
	}
	var exists bool
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := q.AnyLiveBatchWithStockForProduct(ctx, pgUUID(pid))
		if err != nil {
			return fmt.Errorf("batch repo: any live with stock: %w", err)
		}
		exists = got
		return nil
	})
	return exists, err
}

// ----- Helpers ---------------------------------------------------------------

func loadBatch(ctx context.Context, q *db.Queries, id batch.ID) (*batch.Batch, error) {
	bid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("batch repo: parse id %q: %w", id, err)
	}
	row, err := q.GetBatchByID(ctx, pgUUID(bid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, batch.ErrNotFound
		}
		return nil, fmt.Errorf("batch repo: get by id: %w", err)
	}
	return rowToBatch(row)
}

func insertBatchRow(ctx context.Context, q *db.Queries, b *batch.Batch) error {
	bid, _ := uuid.Parse(b.ID().String())
	pid, _ := uuid.Parse(b.ProductID().String())
	tid, _ := uuid.Parse(b.TenantID().String())
	err := q.InsertBatch(ctx, db.InsertBatchParams{
		ID:                         pgUUID(bid),
		ProductID:                  pgUUID(pid),
		TenantID:                   pgUUID(tid),
		BatchNumber:                b.BatchNumber(),
		ManufactureDate:            pgDate(b.ManufactureDate()),
		ExpiryDate:                 pgDate(b.ExpiryDate()),
		ManufacturerName:           b.ManufacturerName(),
		ManufacturingLicenceNumber: b.ManufacturingLicenceNumber(),
		MrpPaise:                   b.MRPPaise(),
		PurchasePricePaise:         b.PurchasePricePaise(),
		QuantityOnHand:             b.QuantityOnHand(),
		Version:                    b.Version(),
		CreatedAt:                  pgRequiredTimestamp(b.CreatedAt()),
		UpdatedAt:                  pgRequiredTimestamp(b.UpdatedAt()),
	})
	if err != nil {
		if isBatchNumberUniqueViolation(err) {
			return batch.ErrBatchNumberTaken
		}
		if isFKViolation(err) {
			// Composite-FK on (product_id, tenant_id) → products(id, tenant_id).
			// Surfaces as cross-tenant product mix-up OR missing parent.
			return product.ErrNotFound
		}
		return fmt.Errorf("batch repo: insert: %w", err)
	}
	return nil
}

// persistBatchState writes the mutable Batch state under the WHERE
// version = $expected predicate + bumps the version. Returns the
// rows-affected count so the caller can branch on 0 → conflict.
func persistBatchState(ctx context.Context, q *db.Queries, b *batch.Batch, expectedVersion int64) (int64, error) {
	bid, _ := uuid.Parse(b.ID().String())
	var deletedAt pgtype.Timestamptz
	var deletedBy *string
	if b.IsDeleted() {
		deletedAt = pgRequiredTimestamp(b.DeletedAt())
		dbStr := b.DeletedBy()
		deletedBy = &dbStr
	}
	rowsAffected, err := q.UpdateBatchWithVersionCheck(ctx, db.UpdateBatchWithVersionCheckParams{
		ID:                         pgUUID(bid),
		QuantityOnHand:             b.QuantityOnHand(),
		Version:                    b.Version(),
		UpdatedAt:                  pgRequiredTimestamp(b.UpdatedAt()),
		IsDeleted:                  b.IsDeleted(),
		DeletedAt:                  deletedAt,
		DeletedBy:                  deletedBy,
		BatchNumber:                b.BatchNumber(),
		ManufacturerName:           b.ManufacturerName(),
		ManufacturingLicenceNumber: b.ManufacturingLicenceNumber(),
		VersionExpected:            expectedVersion,
	})
	if err != nil {
		return 0, fmt.Errorf("batch repo: update: %w", err)
	}
	return rowsAffected, nil
}

func drainBatchEvents(ctx context.Context, tx pgx.Tx, b *batch.Batch) error {
	evs := b.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(b.TenantID().String())
	if err != nil {
		return err
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("batch repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

func rowToBatch(row db.InventoryBatch) (*batch.Batch, error) {
	bid := batch.ID(uuidFromPg(row.ID).String())
	pid := product.ID(uuidFromPg(row.ProductID).String())
	tid := tenant.ID(uuidFromPg(row.TenantID).String())
	deletedBy := ""
	if row.DeletedBy != nil {
		deletedBy = *row.DeletedBy
	}
	return batch.UnmarshalFromDB(batch.Snapshot{
		ID:                         bid,
		ProductID:                  pid,
		TenantID:                   tid,
		BatchNumber:                row.BatchNumber,
		ManufactureDate:            timeFromPgDate(row.ManufactureDate),
		ExpiryDate:                 timeFromPgDate(row.ExpiryDate),
		ManufacturerName:           row.ManufacturerName,
		ManufacturingLicenceNumber: row.ManufacturingLicenceNumber,
		MRPPaise:                   row.MrpPaise,
		PurchasePricePaise:         row.PurchasePricePaise,
		QuantityOnHand:             row.QuantityOnHand,
		Version:                    row.Version,
		CreatedAt:                  timeFromPg(row.CreatedAt),
		UpdatedAt:                  timeFromPg(row.UpdatedAt),
		IsDeleted:                  row.IsDeleted,
		DeletedAt:                  timeFromPg(row.DeletedAt),
		DeletedBy:                  deletedBy,
	}), nil
}

const constraintBatchNumber = "uq_batches_product_number_live"

// isBatchNumberUniqueViolation reports whether err wraps a unique-
// constraint violation on the (product_id, batch_number) partial index.
func isBatchNumberUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	if pgErr.Code != pg.SQLStateUniqueViolation {
		return false
	}
	return pgErr.ConstraintName == constraintBatchNumber
}

// isFKViolation reports whether err wraps a Postgres foreign-key
// violation (SQLSTATE 23503). Used to surface the composite-FK breach
// from batches.fk_batches_product_same_tenant as a friendly
// product.ErrNotFound.
func isFKViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	return pgErr.Code == pg.SQLStateForeignKeyViolation
}
