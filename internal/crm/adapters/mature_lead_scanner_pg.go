package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/crm/adapters/db"
	"github.com/leadkart/leadkart-go/internal/crm/app/jobs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// MatureLeadScannerPG is the pgx/sqlc-backed implementation of
// [jobs.LeadScanner]. Runs cross-tenant under TxScopePlatform per ADR
// 0006 — the scan reads from EVERY tenant's crm.crm_leads in one go;
// scoping per-tenant would require an N+1 query pattern.
//
// The scanner is read-only — it does NOT write reminders. The
// MatureLeadScanWorker iterates the returned candidates + invokes the
// CreateReminderHandler per row, which opens its own tenant-scoped tx
// for the write.
type MatureLeadScannerPG struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewMatureLeadScannerPG wires the scanner.
func NewMatureLeadScannerPG(pool *pgxpool.Pool, tx *pg.Transactor) *MatureLeadScannerPG {
	return &MatureLeadScannerPG{pool: pool, tx: tx, q: db.New(pool)}
}

// Compile-time interface conformance gate.
var _ jobs.LeadScanner = (*MatureLeadScannerPG)(nil)

// MatureLeads runs the cross-tenant scan under TxScopePlatform.
// Returns the per-lead tenant_id so the worker can dispatch reminder
// writes under the correct tenant scope.
func (s *MatureLeadScannerPG) MatureLeads(ctx context.Context, cutoff time.Time, limit int) ([]jobs.MatureLeadCandidate, error) {
	if limit <= 0 {
		limit = jobs.MatureLeadScanLimit
	}
	var out []jobs.MatureLeadCandidate
	err := s.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		rows, err := q.ListConvertedLeadsForMatureScanAllTenants(ctx, db.ListConvertedLeadsForMatureScanAllTenantsParams{
			ConvertedAt: pgconv.PgRequiredTimestamp(cutoff),
			Limit:       int32(limit), //nolint:gosec // limit is caller-controlled + capped by jobs.MatureLeadScanLimit
		})
		if err != nil {
			return fmt.Errorf("crm mature-lead scanner: query: %w", err)
		}
		out = make([]jobs.MatureLeadCandidate, 0, len(rows))
		for _, row := range rows {
			out = append(out, jobs.MatureLeadCandidate{
				TenantID:               tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
				LeadID:                 crmlead.ID(pgconv.UUIDFromPg(row.ID).String()),
				AssignedToMembershipID: uuidStringOrEmpty(row.AssigneeMembershipID),
				ConvertedAt:            pgconv.TimeFromPg(row.ConvertedAt),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
