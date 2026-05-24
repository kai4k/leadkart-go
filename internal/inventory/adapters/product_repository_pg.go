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
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// ProductRepository is the pgx/sqlc-backed implementation of
// [product.Repository]. Tenant-scoped — every method runs under
// TxScopeTenant so the connection's `app.tenant_id` GUC binds before
// queries; Postgres RLS does the filter.
//
// Domain ↔ row mapping lives in this package; sqlc-generated *db.Queries
// hold the SQL.
type ProductRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewProductRepository wires the repository.
func NewProductRepository(pool *pgxpool.Pool, tx *pg.Transactor) *ProductRepository {
	return &ProductRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [product.Repository]. Joins surrounding UoW tx via
// pg.TxFromContext when present.
func (r *ProductRepository) Add(ctx context.Context, p *product.Product) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, p)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, p)
	})
}

func (r *ProductRepository) addOnTx(ctx context.Context, tx pgx.Tx, p *product.Product) error {
	q := r.q.WithTx(tx)
	if err := insertProductRow(ctx, q, p); err != nil {
		return err
	}
	return drainProductEvents(ctx, tx, p)
}

// UpdateByID satisfies [product.Repository] — TDL UpdateFn pattern.
func (r *ProductRepository) UpdateByID(ctx context.Context, id product.ID, updateFn func(*product.Product) (bool, error)) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

func (r *ProductRepository) updateOnTx(ctx context.Context, tx pgx.Tx, id product.ID, updateFn func(*product.Product) (bool, error)) error {
	q := r.q.WithTx(tx)
	p, err := loadProduct(ctx, q, id)
	if err != nil {
		return err
	}
	shouldPersist, err := updateFn(p)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	if err := persistProductState(ctx, q, p); err != nil {
		return err
	}
	return drainProductEvents(ctx, tx, p)
}

// GetByID satisfies [product.Repository].
func (r *ProductRepository) GetByID(ctx context.Context, id product.ID) (*product.Product, error) {
	var out *product.Product
	err := r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := loadProduct(ctx, q, id)
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

// ListPage satisfies [product.Repository]. Keyset paginated per ADR
// 0038. First-page cursor (zero SortValue + empty ID) maps to
// effective MAX values so the (created_at, id) < ($, $) predicate
// matches every row.
func (r *ProductRepository) ListPage(ctx context.Context, tenantID tenant.ID, filter product.ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*product.Product], error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return pagination.Page[*product.Product]{}, fmt.Errorf("product repo: parse tenant id: %w", err)
	}

	// Cursor → SQL parameters. Empty cursor (first page) uses sentinel
	// "infinity" values so `(created_at, id) < (sort, id)` matches all.
	cursorCreatedAt := pgRequiredTimestamp(maxCursorTime())
	var cursorID pgtype.UUID
	cursorID = pgUUID(maxCursorUUID())
	if cursor.ID != "" {
		cursorCreatedAt = pgRequiredTimestamp(cursor.SortValue)
		uid, perr := uuid.Parse(cursor.ID)
		if perr != nil {
			return pagination.Page[*product.Product]{}, fmt.Errorf("product repo: parse cursor id: %w", perr)
		}
		cursorID = pgUUID(uid)
	}

	var out []*product.Product
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListProductsByTenantPage(ctx, db.ListProductsByTenantPageParams{
			TenantID:        pgUUID(tid),
			CursorCreatedAt: cursorCreatedAt,
			CursorID:        cursorID,
			ActiveOnly:      filter.ActiveOnly,
			Search:          filter.Search,
			//nolint:gosec // pageSize is clamped to MaxPageSize (200) by ClampPageSize
			Limit: int32(pageSize + 1),
		})
		if err != nil {
			return fmt.Errorf("product repo: list page: %w", err)
		}
		for _, row := range rows {
			p, perr := rowToProduct(row)
			if perr != nil {
				return perr
			}
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return pagination.Page[*product.Product]{}, err
	}
	page := pagination.BuildPage(out, pageSize, func(p *product.Product) pagination.Cursor {
		return pagination.Cursor{SortValue: p.CreatedAt(), ID: p.ID().String()}
	})
	if page.Items == nil {
		page.Items = []*product.Product{}
	}
	return page, nil
}

// ----- Helpers ---------------------------------------------------------------

func loadProduct(ctx context.Context, q *db.Queries, id product.ID) (*product.Product, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("product repo: parse id %q: %w", id, err)
	}
	row, err := q.GetProductByID(ctx, pgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, product.ErrNotFound
		}
		return nil, fmt.Errorf("product repo: get by id: %w", err)
	}
	return rowToProduct(row)
}

func insertProductRow(ctx context.Context, q *db.Queries, p *product.Product) error {
	pid, err := uuid.Parse(p.ID().String())
	if err != nil {
		return fmt.Errorf("product repo: parse id: %w", err)
	}
	tid, err := uuid.Parse(p.TenantID().String())
	if err != nil {
		return fmt.Errorf("product repo: parse tenant id: %w", err)
	}
	err = q.InsertProduct(ctx, db.InsertProductParams{
		ID:           pgUUID(pid),
		TenantID:     pgUUID(tid),
		Sku:          p.SKU(),
		Name:         p.Name(),
		DosageForm:   p.DosageForm(),
		PackSize:     p.PackSize(),
		HsnCode:      p.HSNCode(),
		//nolint:gosec // bounded by aggregate invariant
		GstRateBps:   int32(p.GSTRateBps()),
		Manufacturer: p.Manufacturer(),
		IsActive:     p.IsActive(),
		CreatedAt:    pgRequiredTimestamp(p.CreatedAt()),
		UpdatedAt:    pgRequiredTimestamp(p.UpdatedAt()),
	})
	if err != nil {
		if isSKUUniqueViolation(err) {
			return product.ErrSKUTaken
		}
		return fmt.Errorf("product repo: insert: %w", err)
	}
	return nil
}

func persistProductState(ctx context.Context, q *db.Queries, p *product.Product) error {
	pid, err := uuid.Parse(p.ID().String())
	if err != nil {
		return fmt.Errorf("product repo: parse id: %w", err)
	}
	if p.IsDeleted() {
		err = q.SoftDeleteProduct(ctx, db.SoftDeleteProductParams{
			ID:        pgUUID(pid),
			DeletedAt: pgRequiredTimestamp(p.DeletedAt()),
			DeletedBy: stringPtrFromValue(p.DeletedBy()),
			UpdatedAt: pgRequiredTimestamp(p.UpdatedAt()),
		})
		if err != nil {
			return fmt.Errorf("product repo: soft delete: %w", err)
		}
		return nil
	}
	err = q.UpdateProduct(ctx, db.UpdateProductParams{
		ID:           pgUUID(pid),
		Name:         p.Name(),
		//nolint:gosec // bounded by aggregate invariant
		GstRateBps:   int32(p.GSTRateBps()),
		Manufacturer: p.Manufacturer(),
		IsActive:     p.IsActive(),
		UpdatedAt:    pgRequiredTimestamp(p.UpdatedAt()),
	})
	if err != nil {
		if isSKUUniqueViolation(err) {
			return product.ErrSKUTaken
		}
		return fmt.Errorf("product repo: update: %w", err)
	}
	return nil
}

func rowToProduct(row db.InventoryProduct) (*product.Product, error) {
	pid := product.ID(uuidFromPg(row.ID).String())
	tid := tenant.ID(uuidFromPg(row.TenantID).String())
	deletedBy := ""
	if row.DeletedBy != nil {
		deletedBy = *row.DeletedBy
	}
	return product.UnmarshalFromDB(product.Snapshot{
		ID:           pid,
		TenantID:     tid,
		SKU:          row.Sku,
		Name:         row.Name,
		DosageForm:   row.DosageForm,
		PackSize:     row.PackSize,
		HSNCode:      row.HsnCode,
		GSTRateBps:   int(row.GstRateBps),
		Manufacturer: row.Manufacturer,
		IsActive:     row.IsActive,
		CreatedAt:    timeFromPg(row.CreatedAt),
		UpdatedAt:    timeFromPg(row.UpdatedAt),
		IsDeleted:    row.IsDeleted,
		DeletedAt:    timeFromPg(row.DeletedAt),
		DeletedBy:    deletedBy,
	}), nil
}

// drainProductEvents pulls events off the aggregate, maps each through
// the integrationevents mapper, and writes the V1 records to the outbox.
func drainProductEvents(ctx context.Context, tx pgx.Tx, p *product.Product) error {
	evs := p.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(p.TenantID().String())
	if err != nil {
		return err
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("product repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

// constraintProductTenantSku names the partial unique index protecting
// (tenant_id, sku) WHERE NOT is_deleted. Migration 20260603000001
// declares `uq_products_tenant_sku_live`.
const constraintProductTenantSku = "uq_products_tenant_sku_live"

// isSKUUniqueViolation reports whether err wraps a Postgres unique-
// constraint violation on the SKU partial unique index.
func isSKUUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	if pgErr.Code != pg.SQLStateUniqueViolation {
		return false
	}
	return pgErr.ConstraintName == constraintProductTenantSku
}
