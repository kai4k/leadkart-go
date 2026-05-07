package impersonation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// AuditEntry is one persistent activity row written for each request
// the operator runs under an impersonation session. Mirrors the .NET
// `AdminImpersonationAudit` Marten document shape.
type AuditEntry struct {
	ID             string
	SessionID      string // empty when system jobs / tests bypass middleware
	OperatorUserID string
	TargetTenantID string
	CorrelationID  string
	HTTPRoute      string
	HTTPMethod     string
	Reason         string
	StartedAtUTC   time.Time
	IsGodMode      bool // true for SuperUser actions per multi-tenancy.md "SuperUser god-mode"
}

// AuditWriter persists [AuditEntry] rows. Implementations:
//   - PgAuditWriter writes to buildingblocks.admin_impersonation_audit.
//   - NoopAuditWriter swallows everything (tests / dev where the
//     audit table isn't migrated).
type AuditWriter interface {
	Write(ctx context.Context, entry AuditEntry) error
}

// PgAuditWriter is the production-path adapter.
type PgAuditWriter struct {
	pool *pgxpool.Pool
}

// NewPgAuditWriter wires the writer against a pgxpool. The
// admin_impersonation_audit table is non-RLS — no tenant context
// needed for the INSERT.
func NewPgAuditWriter(pool *pgxpool.Pool) *PgAuditWriter {
	if pool == nil {
		panic("impersonation: NewPgAuditWriter pool required")
	}
	return &PgAuditWriter{pool: pool}
}

// Write satisfies [AuditWriter].
func (w *PgAuditWriter) Write(ctx context.Context, e AuditEntry) error {
	if e.ID == "" {
		e.ID = ids.NewV7().String()
	}
	if e.StartedAtUTC.IsZero() {
		e.StartedAtUTC = time.Now().UTC()
	}
	id, err := uuid.Parse(e.ID)
	if err != nil {
		return fmt.Errorf("audit: parse id: %w", err)
	}
	op, err := uuid.Parse(e.OperatorUserID)
	if err != nil {
		return fmt.Errorf("audit: parse operator id: %w", err)
	}
	tenant, err := uuid.Parse(e.TargetTenantID)
	if err != nil {
		return fmt.Errorf("audit: parse target tenant id: %w", err)
	}
	var sessionID any
	if e.SessionID != "" {
		sid, perr := uuid.Parse(e.SessionID)
		if perr != nil {
			return fmt.Errorf("audit: parse session id: %w", perr)
		}
		sessionID = sid
	}
	_, err = w.pool.Exec(ctx, `
		INSERT INTO buildingblocks.admin_impersonation_audit
		    (id, session_id, operator_user_id, target_tenant_id,
		     correlation_id, http_route, http_method, reason,
		     started_at_utc, is_god_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, id, sessionID, op, tenant,
		e.CorrelationID, e.HTTPRoute, e.HTTPMethod, e.Reason,
		e.StartedAtUTC, e.IsGodMode)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// NoopAuditWriter swallows every Write. Used in tests + bootstrap
// before the buildingblocks schema is migrated.
type NoopAuditWriter struct{}

// Write satisfies [AuditWriter].
func (NoopAuditWriter) Write(_ context.Context, _ AuditEntry) error { return nil }

var _ AuditWriter = (*PgAuditWriter)(nil)
var _ AuditWriter = NoopAuditWriter{}
