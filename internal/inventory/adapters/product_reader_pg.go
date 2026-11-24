package adapters

import (
	"context"
	"errors"
	"fmt"
	"github.com/leadkart/leadkart-go/internal/common/lifecycle"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

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
func (r *ProductReader) GetProduct(ctx context.Context, tenantID tenant.ID, id product.ID, w sellability.Window) (query.ProductView, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return query.ProductView{}, fmt.Errorf("product reader: parse id %q: %w", id, err)
	}
	var out query.ProductView
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, qerr := r.q.WithTx(tx).GetProductStockByID(ctx, db.GetProductStockByIDParams{
			ID:          pgconv.PgUUID(uid),
			MinSellable: pgconv.PgDate(w.MinSellable),
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
func (r *ProductReader) ListProducts(ctx context.Context, tenantID tenant.ID, filter query.ProductListFilter, w sellability.Window, cursor pagination.Cursor, limit int) ([]query.ProductView, error) {
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
	// "Expiring within n days" is a window on the LAST SELLABLE day, not on the
	// printed expiry: a batch qualifies when it is still sellable and its last
	// sellable day is on or before today + n (common/expiry.LatestExpiryBy).
	expiringFilter := filter.ExpiringWithinDays != nil && *filter.ExpiringWithinDays >= 0
	expiringUpper := w.MinSellable
	if expiringFilter {
		expiringUpper = expiry.LatestExpiryBy(w.Today.AddDate(0, 0, *filter.ExpiringWithinDays), w.StopMonthsBefore)
	}

	out := []query.ProductView{}
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, qerr := r.q.WithTx(tx).ListProductStockPage(ctx, db.ListProductStockPageParams{
			TenantID:        pgconv.PgUUID(tid),
			MinSellable:     pgconv.PgDate(w.MinSellable),
			IncludeActive:   filter.Status.IncludesActive(),
			IncludeInactive: filter.Status.IncludesInactive(),
			Search:          filter.Search,
			Category:        filter.Category,
			DosageForm:      filter.DosageForm,
			Manufacturer:    filter.Manufacturer,
			LowStock:        filter.LowStock,
			ExpiringFilter:  expiringFilter,
			ExpiringUpper:   pgconv.PgDate(expiringUpper),
			CursorCreatedAt: pgconv.PgRequiredTimestamp(cursorCreatedAt),
			CursorID:        pgconv.PgUUID(cursorID),
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

		GSTRateID:            uuidString(row.GstRateID),
		DefaultSaleRatePaise: row.DefaultSaleRatePaise,
		GenericName:          row.GenericName,
		Composition:          row.Composition,
		StorageCondition:     row.StorageCondition,
		DrugScheduleClassID:  uuidString(row.DrugScheduleClassID),

		StockOnHand:      row.StockOnHand,
		StockReserved:    row.StockReserved,
		StockAvailable:   row.StockAvailable,
		StockUnsellable:  row.StockUnsellable,
		StockQuarantined: row.StockQuarantined,
	}
	if row.EarliestExpiry.Valid {
		earliest := pgconv.TimeFromPgDate(row.EarliestExpiry)
		v.EarliestExpiry = &earliest
	}
	return v
}

// CountByStatus satisfies [query.ProductReader]: the product list's status chips, one grouped read.
func (r *ProductReader) CountByStatus(ctx context.Context, tenantID tenant.ID) (lifecycle.Counts, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return lifecycle.Counts{}, fmt.Errorf("product reader: parse tenant id: %w", err)
	}
	var out lifecycle.Counts
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, qerr := r.q.WithTx(tx).CountProductsByStatus(ctx, pgconv.PgUUID(tid))
		if qerr != nil {
			return fmt.Errorf("product reader: count by status: %w", qerr)
		}
		out = lifecycle.Counts{Active: int(row.Active), Inactive: int(row.Inactive)}
		return nil
	})
	return out, err
}
