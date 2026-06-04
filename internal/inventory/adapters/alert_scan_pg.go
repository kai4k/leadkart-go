package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/app/jobs"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// AlertScanRepository is the adapter-side concrete that backs the
// inventory/app/jobs scan workers. Owns all pgx/sqlc concerns so the
// app layer stays boundary-clean (ADR 0047).
//
// Implements the [jobs.AlertScanRepo] consumer-side interface.
type AlertScanRepository struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// Compile-time interface conformance.
var _ jobs.AlertScanRepo = (*AlertScanRepository)(nil)

// NewAlertScanRepository wires the adapter.
func NewAlertScanRepository(pool *pgxpool.Pool) *AlertScanRepository {
	return &AlertScanRepository{pool: pool, q: db.New(pool)}
}

// ListTenants returns every tenant with at least one live product.
func (r *AlertScanRepository) ListTenants(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.q.ListTenantsWithProducts(ctx)
	if err != nil {
		return nil, fmt.Errorf("alert scan repo: list tenants: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, p := range rows {
		out = append(out, uuid.UUID(p.Bytes))
	}
	return out, nil
}

// ListBatchesNearExpiry returns batches whose expiry falls within their
// product's expiry_alert_threshold_days from today.
func (r *AlertScanRepository) ListBatchesNearExpiry(ctx context.Context, tenantID uuid.UUID, today time.Time) ([]jobs.BatchExpiring, error) {
	rows, err := r.q.ListBatchesNearExpiryForTenant(ctx, db.ListBatchesNearExpiryForTenantParams{
		TenantID: pgconv.PgUUID(tenantID),
		Today:    pgconv.PgDate(today),
	})
	if err != nil {
		return nil, fmt.Errorf("alert scan repo: near expiry: %w", err)
	}
	out := make([]jobs.BatchExpiring, 0, len(rows))
	for _, row := range rows {
		out = append(out, jobs.BatchExpiring{
			TenantID:      uuid.UUID(row.TenantID.Bytes),
			ProductID:     uuid.UUID(row.ProductID.Bytes),
			BatchID:       uuid.UUID(row.ID.Bytes),
			BatchNumber:   row.BatchNumber,
			ExpiryDate:    row.ExpiryDate.Time.UTC(),
			ThresholdDays: int(row.ExpiryAlertThresholdDays),
		})
	}
	return out, nil
}

// ListProductsBelowReorder returns products whose live + not-expired
// stock-on-hand is strictly below reorder_level.
func (r *AlertScanRepository) ListProductsBelowReorder(ctx context.Context, tenantID uuid.UUID, today time.Time) ([]jobs.ReorderProduct, error) {
	rows, err := r.q.ListProductsBelowReorderForTenant(ctx, db.ListProductsBelowReorderForTenantParams{
		TenantID: pgconv.PgUUID(tenantID),
		Today:    pgconv.PgDate(today),
	})
	if err != nil {
		return nil, fmt.Errorf("alert scan repo: below reorder: %w", err)
	}
	out := make([]jobs.ReorderProduct, 0, len(rows))
	for _, row := range rows {
		out = append(out, jobs.ReorderProduct{
			TenantID:     tenantID,
			ProductID:    uuid.UUID(row.ID.Bytes),
			SKU:          row.Sku,
			ReorderLevel: int(row.ReorderLevel),
			StockOnHand:  row.StockOnHand,
		})
	}
	return out, nil
}

// EmitIfNew atomically dedups + writes the integration event to the
// inventory outbox. Returns (true, nil) when newly emitted; (false, nil)
// when the dedup ledger already contained the (tenant, kind, subject,
// day) tuple.
func (r *AlertScanRepository) EmitIfNew(ctx context.Context, tenantID uuid.UUID, kind string, subjectID uuid.UUID, today time.Time, event integrationevents.Event) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("alert scan repo: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := r.q.WithTx(tx)
	inserted, err := q.InsertAlertEmission(ctx, db.InsertAlertEmissionParams{
		TenantID:    pgconv.PgUUID(tenantID),
		Kind:        kind,
		SubjectID:   pgconv.PgUUID(subjectID),
		EmittedDate: pgconv.PgDate(today),
	})
	if err != nil {
		return false, fmt.Errorf("alert scan repo: dedup insert: %w", err)
	}
	if inserted == 0 {
		return false, tx.Commit(ctx)
	}

	// Write to the shared common.outbox relay in the same tx (ADR 0064/0067) —
	// the per-module inventory.outbox was retired.
	if err := writeOutboxEvents(ctx, tx, tenantID, []integrationevents.Event{event}); err != nil {
		return false, fmt.Errorf("alert scan repo: outbox write: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("alert scan repo: commit: %w", err)
	}
	return true, nil
}
