// audit_reader_pg.go — concrete pg-backed [audit.Reader].
//
// Lives in the adapters package (where the sqlc-generated db.* package
// is allowed to be imported) per ADR 0047 boundary discipline. The
// consumer-side interface [audit.Reader] is defined in
// internal/platform/audit/reader.go.

package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/audit"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// AuditReaderPG is the concrete pg-backed implementation of
// [audit.Reader]. Runs every query under [pg.TxScopePlatform] — the
// audit table is operator-facing, no RLS; HTTP boundary is responsible
// for authorisation.
type AuditReaderPG struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewAuditReaderPG wires the reader. Panics on nil pool / nil tx — both
// are load-bearing for the platform-scope tx.
func NewAuditReaderPG(pool *pgxpool.Pool, tx *pg.Transactor) *AuditReaderPG {
	if pool == nil {
		panic("adapters: NewAuditReaderPG pool required")
	}
	if tx == nil {
		panic("adapters: NewAuditReaderPG transactor required")
	}
	return &AuditReaderPG{pool: pool, tx: tx, q: db.New(pool)}
}

// Compile-time interface satisfaction.
var _ audit.Reader = (*AuditReaderPG)(nil)

// ListByTenant returns up to limit entries scoped to tenantID, ordered
// (occurred_at_utc DESC, id DESC) keyset-paginated.
func (r *AuditReaderPG) ListByTenant(
	ctx context.Context,
	tenantID uuid.UUID,
	before time.Time,
	beforeID uuid.UUID,
	limit int32,
) ([]audit.Entry, error) {
	var rows []db.BuildingblocksAuditLogEntry
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		out, qerr := r.q.WithTx(tx).ListAuditEventsByTenantPage(ctx, db.ListAuditEventsByTenantPageParams{
			TenantID:       pgUUID(tenantID),
			BeforeOccurred: pgRequiredTimestamp(before),
			BeforeID:       pgUUID(beforeID),
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

// ListByUser returns up to limit entries scoped to userID (= JWT subject
// = person_id), ordered (occurred_at_utc DESC, id DESC).
func (r *AuditReaderPG) ListByUser(
	ctx context.Context,
	userID uuid.UUID,
	before time.Time,
	beforeID uuid.UUID,
	limit int32,
) ([]audit.Entry, error) {
	var rows []db.BuildingblocksAuditLogEntry
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		out, qerr := r.q.WithTx(tx).ListAuditEventsByUserPage(ctx, db.ListAuditEventsByUserPageParams{
			UserID:         pgUUID(userID),
			BeforeOccurred: pgRequiredTimestamp(before),
			BeforeID:       pgUUID(beforeID),
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

func rowsToEntries(rows []db.BuildingblocksAuditLogEntry) []audit.Entry {
	out := make([]audit.Entry, 0, len(rows))
	for _, r := range rows {
		e := audit.Entry{
			ID:            uuidFromPg(r.ID),
			Action:        r.Action,
			UserID:        uuidFromPg(r.UserID),
			TenantID:      uuidFromPg(r.TenantID),
			CorrelationID: uuidFromPg(r.CorrelationID),
			OccurredAtUTC: timeFromPg(r.OccurredAtUtc),
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
