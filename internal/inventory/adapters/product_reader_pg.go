package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// noExpiryFilter is the neutral value of the expiring_within_days SQL argument.
const noExpiryFilter = -1

// ProductReader is the pgx/sqlc-backed [query.ProductReader]: the product
// screens' read model. Each method is ONE statement that returns the product
// rows together with their stock totals (a lateral aggregate over the
// product's batches), so a page of N products costs one query, not N+1.
// Tenant-scoped via TxScopeTenant: every read opens its tx with
// WithinTxPgxTenant and the explicit tenantID (ADR 0062), so RLS filters both
// tables to the caller's tenant.
type ProductReader struct {
	tx *pg.Transactor
	q  *db.Queries
}

// NewProductReader constructs a ProductReader.
func NewProductReader(pool *pgxpool.Pool, tx *pg.Transactor) *ProductReader {
	return &ProductReader{tx: tx, q: db.New(pool)}
}

var _ query.ProductReader = (*ProductReader)(nil)

// GetProduct satisfies [query.ProductReader].
func (r *ProductReader) GetProduct(ctx context.Context, tenantID tenant.ID, id product.ID, today time.Time) (query.ProductView, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return query.ProductView{}, fmt.Errorf("product reader: parse id %q: %w", id, err)
	}
	var out query.ProductView
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, qerr := r.q.WithTx(tx).GetProductStockByID(ctx, db.GetProductStockByIDParams{
			ID:    pgconv.PgUUID(uid),
			Today: pgconv.PgDate(today),
		})
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return product.ErrNotFound
			}
			return fmt.Errorf("product reader: get: %w", qerr)
		}
		// The two statements select the same columns; one mapper serves both.
		out = productStockRowToView(db.ListProductStockPageRow(row))
		return nil
	})
	if err != nil {
		return query.ProductView{}, err
	}
	return out, nil
}

// ListProducts satisfies [query.ProductReader]. An empty cursor maps to the
// sentinel MAX values so the keyset predicate matches every row.
func (r *ProductReader) ListProducts(ctx context.Context, tenantID tenant.ID, filter query.ProductListFilter, today time.Time, cursor pagination.Cursor, limit int) ([]query.ProductView, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return nil, fmt.Errorf("product reader: parse tenant id: %w", err)
	}
	cursorCreatedAt, cursorID := maxCursorTime(), maxCursorUUID()
	if cursor.ID != "" {
		uid, perr := uuid.Parse(cursor.ID)
		if perr != nil {
			return nil, fmt.Errorf("product reader: parse cursor id: %w", perr)
		}
		cursorCreatedAt, cursorID = cursor.SortValue, uid
	}
	expiring := int32(noExpiryFilter)
	if filter.ExpiringWithinDays != nil {
		//nolint:gosec // bounded to [0, 36500] by the HTTP layer; a negative value only switches the filter off
		expiring = int32(*filter.ExpiringWithinDays)
	}

	out := []query.ProductView{}
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, qerr := r.q.WithTx(tx).ListProductStockPage(ctx, db.ListProductStockPageParams{
			TenantID:           pgconv.PgUUID(tid),
			Today:              pgconv.PgDate(today),
			ActiveOnly:         filter.ActiveOnly,
			Search:             filter.Search,
			Category:           filter.Category,
			DosageForm:         filter.DosageForm,
			Manufacturer:       filter.Manufacturer,
			LowStock:           filter.LowStock,
			ExpiringWithinDays: expiring,
			CursorCreatedAt:    pgconv.PgRequiredTimestamp(cursorCreatedAt),
			CursorID:           pgconv.PgUUID(cursorID),
			//nolint:gosec // limit is a clamped page size + 1 (at most 201)
			Limit: int32(limit),
		})
		if qerr != nil {
			return fmt.Errorf("product reader: list: %w", qerr)
		}
		for _, row := range rows {
			out = append(out, productStockRowToView(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// productStockRowToView maps one read-model row to the app-layer View.
func productStockRowToView(row db.ListProductStockPageRow) query.ProductView {
	category := row.ProductCategory
	if category == "" {
		category = product.ProductCategoryDefault
	}
	v := query.ProductView{
		ID:           pgconv.UUIDFromPg(row.ID).String(),
		TenantID:     pgconv.UUIDFromPg(row.TenantID).String(),
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

		ProductCategory:          category,
		ReorderLevel:             int(row.ReorderLevel),
		ExpiryAlertThresholdDays: int(row.ExpiryAlertThresholdDays),

		StockOnHand:    row.StockOnHand,
		StockReserved:  row.StockReserved,
		StockAvailable: row.StockOnHand - row.StockReserved,
	}
	if row.EarliestExpiry.Valid {
		expiry := pgconv.TimeFromPgDate(row.EarliestExpiry)
		v.EarliestExpiry = &expiry
	}
	return v
}
