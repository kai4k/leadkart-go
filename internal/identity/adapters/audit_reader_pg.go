// audit_reader_pg.go — pg-backed [audit.Reader] (ADR 0047 boundary).
// Consumer-side interface: internal/common/audit/reader.go.

package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
)

// AuditReaderPG implements [audit.Reader] over Postgres. All queries run
// under [pg.TxScopePlatform] — the audit table has no RLS; the HTTP
// boundary enforces authorisation.
type AuditReaderPG struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewAuditReaderPG wires the reader. Panics on nil pool or nil tx.
func NewAuditReaderPG(pool *pgxpool.Pool, tx *pg.Transactor) *AuditReaderPG {
	if pool == nil {
		panic("adapters: NewAuditReaderPG pool required")
	}
	if tx == nil {
		panic("adapters: NewAuditReaderPG transactor required")
	}
	return &AuditReaderPG{pool: pool, tx: tx, q: db.New(pool)}
}

var _ audit.Reader = (*AuditReaderPG)(nil)

// ListByTenant returns up to limit entries for tenantID, keyset-paginated
// (occurred_at_utc DESC, id DESC).
func (r *AuditReaderPG) ListByTenant(
	ctx context.Context,
	tenantID uuid.UUID,
	before time.Time,
	beforeID uuid.UUID,
	limit int32,
) ([]audit.Entry, error) {
	var rows []db.CommonAuditLogEntry
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		out, qerr := r.q.WithTx(tx).ListAuditEventsByTenantPage(ctx, db.ListAuditEventsByTenantPageParams{
			TenantID:       pgconv.PgUUID(tenantID),
			BeforeOccurred: pgconv.PgRequiredTimestamp(before),
			BeforeID:       pgconv.PgUUID(beforeID),
			Limit:          limit,
		})
		if qerr != nil {
			return qerr
		}
		rows = out
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("audit reader: list by tenant: %w", err)
	}
	return rowsToEntries(rows), nil
}

// ListByUser returns up to limit entries for userID (JWT subject = person_id),
// keyset-paginated (occurred_at_utc DESC, id DESC).
func (r *AuditReaderPG) ListByUser(
	ctx context.Context,
	userID uuid.UUID,
	before time.Time,
	beforeID uuid.UUID,
	limit int32,
) ([]audit.Entry, error) {
	var rows []db.CommonAuditLogEntry
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		out, qerr := r.q.WithTx(tx).ListAuditEventsByUserPage(ctx, db.ListAuditEventsByUserPageParams{
			UserID:         pgconv.PgUUID(userID),
			BeforeOccurred: pgconv.PgRequiredTimestamp(before),
			BeforeID:       pgconv.PgUUID(beforeID),
			Limit:          limit,
		})
		if qerr != nil {
			return qerr
		}
		rows = out
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("audit reader: list by user: %w", err)
	}
	return rowsToEntries(rows), nil
}

func rowsToEntries(rows []db.CommonAuditLogEntry) []audit.Entry {
	out := make([]audit.Entry, 0, len(rows))
	for _, r := range rows {
		e := audit.Entry{
			ID:            pgconv.UUIDFromPg(r.ID),
			Action:        r.Action,
			UserID:        pgconv.UUIDFromPg(r.UserID),
			TenantID:      pgconv.UUIDFromPg(r.TenantID),
			CorrelationID: pgconv.UUIDFromPg(r.CorrelationID),
			OccurredAtUTC: pgconv.TimeFromPg(r.OccurredAtUtc),
			Duration:      time.Duration(r.DurationMs) * time.Millisecond,
			Succeeded:     r.Succeeded,
			Payload:       r.Payload,
		}
		if r.FailureReason != nil {
			e.FailureReason = *r.FailureReason
		}
		out = append(out, e)
	}
	return out
}
