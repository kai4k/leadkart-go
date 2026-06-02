package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// BatchRepository is the pgx/sqlc-backed [batch.Repository].
// Tenant-scoped via TxScopeTenant.
//
// UpdateByID uses SELECT ... FOR UPDATE before loading the aggregate
// so concurrent writers serialize at the DB layer; no application
// retry loop is needed. Pessimistic locking is correct for hot-row
// counters (Postgres §13.3.2 + Stripe ledger + DDIA Ch.7). The
// version column is kept as defense-in-depth and audit signal; the
// WHERE version=$expected predicate cannot fail because the row lock
// prevents two writers from sharing an expected version.
type BatchRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewBatchRepository constructs a BatchRepository.
func NewBatchRepository(pool *pgxpool.Pool, tx *pg.Transactor) *BatchRepository {
	return &BatchRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [batch.Repository]. Joins surrounding UoW tx; GUC
// bound from b.TenantID() (ADR 0062).
func (r *BatchRepository) Add(ctx context.Context, b *batch.Batch) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, b)
	}
	return r.tx.WithinTxPgxTenant(ctx, b.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
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

// UpdateByID satisfies [batch.Repository]. Acquires SELECT ... FOR UPDATE
// before loading the aggregate; see [BatchRepository] for rationale.
// GUC bound from tenantID (ADR 0062).
func (r *BatchRepository) UpdateByID(ctx context.Context, tenantID tenant.ID, id batch.ID, updateFn func(*batch.Batch) (bool, error)) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

// lockBatchRowForUpdate acquires SELECT ... FOR UPDATE on the batches row.
// Concurrent callers block until this tx commits or rolls back; Postgres
// releases the lock on tx end or connection drop automatically.
// Returns batch.ErrNotFound for a missing or soft-deleted row.
func (r *BatchRepository) lockBatchRowForUpdate(ctx context.Context, tx pgx.Tx, id batch.ID) error {
	bid, err := uuid.Parse(id.String())
	if err != nil {
		return fmt.Errorf("batch repo: parse id %q: %w", id, err)
	}
	if _, scanErr := r.q.WithTx(tx).LockBatchForUpdate(ctx, pgconv.PgUUID(bid)); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return batch.ErrNotFound
		}
		return fmt.Errorf("batch repo: lock for update: %w", scanErr)
	}
	return nil
}

func (r *BatchRepository) updateOnTx(ctx context.Context, tx pgx.Tx, id batch.ID, updateFn func(*batch.Batch) (bool, error)) error {
	// Acquire row lock first; concurrent writers block until our tx
	// commits, so the version check below can never fail in production
	// (Postgres §13.3.2 lock-then-read pattern).
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
	// WHERE version=$expected is defense-in-depth; rowsAffected==0
	// signals a programmer error (e.g. external SQL bypass), not a
	// real race (the row lock prevents that).
	rowsAffected, err := persistBatchState(ctx, q, b, expectedVersion)
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return batch.ErrConcurrencyConflict
	}
	return drainBatchEvents(ctx, tx, b)
}

// GetByID satisfies [batch.Repository]. GUC bound from tenantID (ADR 0062).
func (r *BatchRepository) GetByID(ctx context.Context, tenantID tenant.ID, id batch.ID) (*batch.Batch, error) {
	var out *batch.Batch
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
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
// (expiry_date DESC, id DESC); GUC bound from tenantID (ADR 0062).
func (r *BatchRepository) ListByProductPage(ctx context.Context, tenantID tenant.ID, productID product.ID, filter batch.ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*batch.Batch], error) {
	pid, err := uuid.Parse(productID.String())
	if err != nil {
		return pagination.Page[*batch.Batch]{}, fmt.Errorf("batch repo: parse product id: %w", err)
	}

	cursorExpiry := pgconv.PgDate(maxCursorTime())
	cursorID := pgconv.PgUUID(maxCursorUUID())
	if cursor.ID != "" {
		cursorExpiry = pgconv.PgDate(cursor.SortValue)
		uid, perr := uuid.Parse(cursor.ID)
		if perr != nil {
			return pagination.Page[*batch.Batch]{}, fmt.Errorf("batch repo: parse cursor id: %w", perr)
		}
		cursorID = pgconv.PgUUID(uid)
	}

	var out []*batch.Batch
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListBatchesByProductPage(ctx, db.ListBatchesByProductPageParams{
			ProductID:        pgconv.PgUUID(pid),
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

// AnyLiveWithStockForProduct satisfies [batch.Repository]. GUC bound
// from tenantID (ADR 0062).
func (r *BatchRepository) AnyLiveWithStockForProduct(ctx context.Context, tenantID tenant.ID, productID product.ID) (bool, error) {
	pid, err := uuid.Parse(productID.String())
	if err != nil {
		return false, fmt.Errorf("batch repo: parse product id: %w", err)
	}
	var exists bool
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := q.AnyLiveBatchWithStockForProduct(ctx, pgconv.PgUUID(pid))
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
	row, err := q.GetBatchByID(ctx, pgconv.PgUUID(bid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, batch.ErrNotFound
		}
		return nil, fmt.Errorf("batch repo: get by id: %w", err)
	}
	return rowToBatch(row)
}

func insertBatchRow(ctx context.Context, q *db.Queries, b *batch.Batch) error {
	bid, err := uuid.Parse(b.ID().String())
	if err != nil {
		return fmt.Errorf("batch repo: parse id: %w", err)
	}
	pid, err := uuid.Parse(b.ProductID().String())
	if err != nil {
		return fmt.Errorf("batch repo: parse product_id: %w", err)
	}
	tid, err := uuid.Parse(b.TenantID().String())
	if err != nil {
		return fmt.Errorf("batch repo: parse tenant_id: %w", err)
	}
	err = q.InsertBatch(ctx, db.InsertBatchParams{
		ID:                         pgconv.PgUUID(bid),
		ProductID:                  pgconv.PgUUID(pid),
		TenantID:                   pgconv.PgUUID(tid),
		BatchNumber:                b.BatchNumber(),
		ManufactureDate:            pgconv.PgDate(b.ManufactureDate()),
		ExpiryDate:                 pgconv.PgDate(b.ExpiryDate()),
		ManufacturerName:           b.ManufacturerName(),
		ManufacturingLicenceNumber: b.ManufacturingLicenceNumber(),
		MrpPaise:                   b.MRPPaise(),
		PurchasePricePaise:         b.PurchasePricePaise(),
		QuantityOnHand:             b.QuantityOnHand(),
		Version:                    b.Version(),
		CreatedAt:                  pgconv.PgRequiredTimestamp(b.CreatedAt()),
		UpdatedAt:                  pgconv.PgRequiredTimestamp(b.UpdatedAt()),
	})
	if err != nil {
		if isBatchNumberUniqueViolation(err) {
			return batch.ErrBatchNumberTaken
		}
		if isFKViolation(err) {
			// Composite-FK (product_id, tenant_id) violation — missing or cross-tenant parent.
			return product.ErrNotFound
		}
		return fmt.Errorf("batch repo: insert: %w", err)
	}
	return nil
}

// persistBatchState writes mutable Batch state with WHERE version=$expected
// and bumps version. Returns rows-affected so the caller can detect 0 → conflict.
func persistBatchState(ctx context.Context, q *db.Queries, b *batch.Batch, expectedVersion int64) (int64, error) {
	bid, err := uuid.Parse(b.ID().String())
	if err != nil {
		return 0, fmt.Errorf("batch repo: parse id: %w", err)
	}
	var deletedAt pgtype.Timestamptz
	var deletedBy *string
	if b.IsDeleted() {
		deletedAt = pgconv.PgRequiredTimestamp(b.DeletedAt())
		deletedBy = pgconv.ZeroToNil(b.DeletedBy())
	}
	rowsAffected, err := q.UpdateBatchWithVersionCheck(ctx, db.UpdateBatchWithVersionCheckParams{
		ID:                         pgconv.PgUUID(bid),
		QuantityOnHand:             b.QuantityOnHand(),
		Version:                    b.Version(),
		UpdatedAt:                  pgconv.PgRequiredTimestamp(b.UpdatedAt()),
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
	bid := batch.ID(pgconv.UUIDFromPg(row.ID).String())
	pid := product.ID(pgconv.UUIDFromPg(row.ProductID).String())
	tid := tenant.ID(pgconv.UUIDFromPg(row.TenantID).String())
	deletedBy := ""
	if row.DeletedBy != nil {
		deletedBy = *row.DeletedBy
	}
	return batch.UnmarshalFromDB(batch.Snapshot{
		ID:                         bid,
		ProductID:                  pid,
		TenantID:                   tid,
		BatchNumber:                row.BatchNumber,
		ManufactureDate:            pgconv.TimeFromPgDate(row.ManufactureDate),
		ExpiryDate:                 pgconv.TimeFromPgDate(row.ExpiryDate),
		ManufacturerName:           row.ManufacturerName,
		ManufacturingLicenceNumber: row.ManufacturingLicenceNumber,
		MRPPaise:                   row.MrpPaise,
		PurchasePricePaise:         row.PurchasePricePaise,
		QuantityOnHand:             row.QuantityOnHand,
		Version:                    row.Version,
		CreatedAt:                  pgconv.TimeFromPg(row.CreatedAt),
		UpdatedAt:                  pgconv.TimeFromPg(row.UpdatedAt),
		IsDeleted:                  row.IsDeleted,
		DeletedAt:                  pgconv.TimeFromPg(row.DeletedAt),
		DeletedBy:                  deletedBy,
	}), nil
}

const constraintBatchNumber = "uq_batches_product_number_live"

// isBatchNumberUniqueViolation reports whether err is a unique-constraint
// violation on the (product_id, batch_number) partial index.
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

// isFKViolation reports whether err is SQLSTATE 23503 (foreign key
// violation), used to surface batches.fk_batches_product_same_tenant
// breaches as product.ErrNotFound.
func isFKViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	return pgErr.Code == pg.SQLStateForeignKeyViolation
}
