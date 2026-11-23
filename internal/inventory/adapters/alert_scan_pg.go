package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/app/jobs"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// AlertScanRepository is the pgx/sqlc-backed [jobs.AlertScanRepo]. The tenant
// list is the one cross-tenant read (platform scope); every other read and the
// ledger-plus-outbox write run under the tenant scope (TxScopeTenant semantics: the GUC is bound from the explicit tenant id) of the tenant they are
// about, so RLS applies exactly as it does on the request path (a pool with no
// scope bound sees no rows under FORCE row level security).
type AlertScanRepository struct {
	tx *pg.Transactor
	q  *db.Queries
}

var _ jobs.AlertScanRepo = (*AlertScanRepository)(nil)

// NewAlertScanRepository constructs the repository.
func NewAlertScanRepository(pool *pgxpool.Pool, tx *pg.Transactor) *AlertScanRepository {
	return &AlertScanRepository{tx: tx, q: db.New(pool)}
}

// ListTenants satisfies [jobs.AlertScanRepo].
func (r *AlertScanRepository) ListTenants(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := r.q.WithTx(tx).ListTenantsWithProducts(ctx)
		if err != nil {
			return err
		}
		for _, p := range rows {
			out = append(out, uuid.UUID(p.Bytes))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("alert scan repo: list tenants: %w", err)
	}
	return out, nil
}

// ListBatchesForExpiryScan satisfies [jobs.AlertScanRepo].
func (r *AlertScanRepository) ListBatchesForExpiryScan(ctx context.Context, tenantID uuid.UUID) ([]jobs.ExpiryCandidate, error) {
	var out []jobs.ExpiryCandidate
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := r.q.WithTx(tx).ListBatchesForExpiryScan(ctx, pgconv.PgUUID(tenantID))
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, jobs.ExpiryCandidate{
				TenantID: tenantID, ProductID: pgconv.UUIDFromPg(row.ProductID), BatchID: pgconv.UUIDFromPg(row.BatchID),
				BatchNumber: row.BatchNumber, ProductSKU: row.Sku, ProductName: row.ProductName,
				ExpiryDate: pgconv.TimeFromPgDate(row.ExpiryDate),
				OnHand:     row.QuantityOnHand, Reserved: row.QuantityReserved, Quarantined: row.QuantityQuarantined,
				ThresholdDays: int(row.ExpiryAlertThresholdDays),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("alert scan repo: expiry candidates: %w", err)
	}
	return out, nil
}

// ListProductsBelowReorder satisfies [jobs.AlertScanRepo].
func (r *AlertScanRepository) ListProductsBelowReorder(ctx context.Context, tenantID uuid.UUID, minSellable time.Time) ([]jobs.ReorderProduct, error) {
	var out []jobs.ReorderProduct
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := r.q.WithTx(tx).ListProductsBelowReorderForTenant(ctx, db.ListProductsBelowReorderForTenantParams{
			TenantID:    pgconv.PgUUID(tenantID),
			MinSellable: pgconv.PgDate(minSellable),
		})
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, jobs.ReorderProduct{
				TenantID: tenantID, ProductID: pgconv.UUIDFromPg(row.ID), SKU: row.Sku,
				ReorderLevel: int(row.ReorderLevel), StockOnHand: row.StockOnHand,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("alert scan repo: below reorder: %w", err)
	}
	return out, nil
}

// EmitIfNew satisfies [jobs.AlertScanRepo]: the ledger insert and the outbox row
// are ONE transaction, so an event is published exactly when its ledger row is new.
func (r *AlertScanRepository) EmitIfNew(ctx context.Context, tenantID uuid.UUID, kind string, subjectID uuid.UUID, emissionKey string, event integrationevents.Event) (bool, error) {
	var inserted bool
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		n, err := r.q.WithTx(tx).InsertAlertEmission(ctx, db.InsertAlertEmissionParams{
			TenantID: pgconv.PgUUID(tenantID), Kind: kind, SubjectID: pgconv.PgUUID(subjectID),
			EmissionKey: emissionKey, EmittedDate: pgconv.PgDate(event.OccurredAt()),
		})
		if err != nil {
			return fmt.Errorf("dedup insert: %w", err)
		}
		if n == 0 {
			return nil
		}
		inserted = true
		return writeOutboxEvents(ctx, tx, tenantID, []integrationevents.Event{event})
	})
	if err != nil {
		return false, fmt.Errorf("alert scan repo: emit: %w", err)
	}
	return inserted, nil
}
