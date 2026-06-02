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
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// ProductRepository is the pgx/sqlc-backed [product.Repository].
// Tenant-scoped via TxScopeTenant; RLS filters rows by app.tenant_id GUC.
// Domain↔row mapping is in this package; SQL lives in *db.Queries.
type ProductRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewProductRepository constructs a ProductRepository.
func NewProductRepository(pool *pgxpool.Pool, tx *pg.Transactor) *ProductRepository {
	return &ProductRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [product.Repository]. Joins surrounding UoW tx when
// present; GUC bound from p.TenantID() (ADR 0062).
func (r *ProductRepository) Add(ctx context.Context, p *product.Product) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, p)
	}
	return r.tx.WithinTxPgxTenant(ctx, p.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
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

// UpdateByID satisfies [product.Repository] (TDL UpdateFn pattern).
// GUC bound from tenantID (ADR 0062).
func (r *ProductRepository) UpdateByID(ctx context.Context, tenantID tenant.ID, id product.ID, updateFn func(*product.Product) (bool, error)) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
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

// GetByID satisfies [product.Repository]. GUC bound from tenantID (ADR 0062).
func (r *ProductRepository) GetByID(ctx context.Context, tenantID tenant.ID, id product.ID) (*product.Product, error) {
	var out *product.Product
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
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

// ListPage satisfies [product.Repository]. Keyset paginated (ADR 0038);
// empty cursor maps to sentinel MAX values so the predicate matches all rows.
func (r *ProductRepository) ListPage(ctx context.Context, tenantID tenant.ID, filter product.ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*product.Product], error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return pagination.Page[*product.Product]{}, fmt.Errorf("product repo: parse tenant id: %w", err)
	}

	// Empty cursor (first page) uses sentinel values so the keyset
	// predicate matches every row.
	cursorCreatedAt := pgconv.PgRequiredTimestamp(maxCursorTime())
	var cursorID pgtype.UUID
	cursorID = pgconv.PgUUID(maxCursorUUID())
	if cursor.ID != "" {
		cursorCreatedAt = pgconv.PgRequiredTimestamp(cursor.SortValue)
		uid, perr := uuid.Parse(cursor.ID)
		if perr != nil {
			return pagination.Page[*product.Product]{}, fmt.Errorf("product repo: parse cursor id: %w", perr)
		}
		cursorID = pgconv.PgUUID(uid)
	}

	var out []*product.Product
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListProductsByTenantPage(ctx, db.ListProductsByTenantPageParams{
			TenantID:        pgconv.PgUUID(tid),
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
	row, err := q.GetProductByID(ctx, pgconv.PgUUID(uid))
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
		ID:         pgconv.PgUUID(pid),
		TenantID:   pgconv.PgUUID(tid),
		Sku:        p.SKU(),
		Name:       p.Name(),
		DosageForm: p.DosageForm(),
		PackSize:   p.PackSize(),
		HsnCode:    p.HSNCode(),
		//nolint:gosec // bounded by aggregate invariant
		GstRateBps:   int32(p.GSTRateBps()),
		Manufacturer: p.Manufacturer(),
		IsActive:     p.IsActive(),
		CreatedAt:    pgconv.PgRequiredTimestamp(p.CreatedAt()),
		UpdatedAt:    pgconv.PgRequiredTimestamp(p.UpdatedAt()),
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
			ID:        pgconv.PgUUID(pid),
			DeletedAt: pgconv.PgRequiredTimestamp(p.DeletedAt()),
			DeletedBy: pgconv.ZeroToNil(p.DeletedBy()),
			UpdatedAt: pgconv.PgRequiredTimestamp(p.UpdatedAt()),
		})
		if err != nil {
			return fmt.Errorf("product repo: soft delete: %w", err)
		}
		return nil
	}
	err = q.UpdateProduct(ctx, db.UpdateProductParams{
		ID:   pgconv.PgUUID(pid),
		Name: p.Name(),
		//nolint:gosec // bounded by aggregate invariant
		GstRateBps:   int32(p.GSTRateBps()),
		Manufacturer: p.Manufacturer(),
		IsActive:     p.IsActive(),
		UpdatedAt:    pgconv.PgRequiredTimestamp(p.UpdatedAt()),
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
	pid := product.ID(pgconv.UUIDFromPg(row.ID).String())
	tid := tenant.ID(pgconv.UUIDFromPg(row.TenantID).String())
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
		CreatedAt:    pgconv.TimeFromPg(row.CreatedAt),
		UpdatedAt:    pgconv.TimeFromPg(row.UpdatedAt),
		IsDeleted:    row.IsDeleted,
		DeletedAt:    pgconv.TimeFromPg(row.DeletedAt),
		DeletedBy:    deletedBy,
	}), nil
}

// drainProductEvents maps and writes the product's pending domain events
// to the outbox within the current transaction.
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

// constraintProductTenantSku is the partial unique index on
// (tenant_id, sku) WHERE NOT is_deleted (migration 20260603000001).
const constraintProductTenantSku = "uq_products_tenant_sku_live"

// isSKUUniqueViolation reports whether err is a unique-constraint
// violation on the SKU partial unique index.
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
