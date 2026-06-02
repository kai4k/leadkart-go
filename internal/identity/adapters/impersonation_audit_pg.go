// impersonation_audit_pg.go — persistent audit writer for the platform
// impersonation flow (ADR 0051, Wave 9.1b).
//
// Types are prefixed ImpersonationAudit* to avoid name collisions in
// the flat adapters package. The interface lives here pending a real
// consumer; when Wave 4.1 wires X-Impersonation-Session-Id →
// operator-correlation rows it should migrate to
// internal/identity/domain/impersonation/ per Cheney. Migration
// 20260507000006 ships the table; row insertions start in Phase 2+.

package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// ImpersonationAuditEntry is one activity row per impersonated request.
// Mirrors the .NET AdminImpersonationAudit document shape.
type ImpersonationAuditEntry struct {
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

// ImpersonationAuditWriter persists [ImpersonationAuditEntry] rows.
// [ImpersonationAuditWriterPG] writes to common.admin_impersonation_audit;
// [ImpersonationAuditWriterNoop] is a no-op for tests and pre-migration dev.
type ImpersonationAuditWriter interface {
	Write(ctx context.Context, entry ImpersonationAuditEntry) error
}

// ImpersonationAuditWriterPG is the production-path adapter.
type ImpersonationAuditWriterPG struct {
	pool *pgxpool.Pool
}

// NewImpersonationAuditWriterPG wires the writer. The table is non-RLS.
func NewImpersonationAuditWriterPG(pool *pgxpool.Pool) *ImpersonationAuditWriterPG {
	if pool == nil {
		panic("impersonation: NewImpersonationAuditWriterPG pool required")
	}
	return &ImpersonationAuditWriterPG{pool: pool}
}

// Write satisfies [ImpersonationAuditWriter]. Auto-populates ID and StartedAtUTC when zero.
func (w *ImpersonationAuditWriterPG) Write(ctx context.Context, e ImpersonationAuditEntry) error {
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
	tenantID, err := uuid.Parse(e.TargetTenantID)
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
		INSERT INTO common.admin_impersonation_audit
		    (id, session_id, operator_user_id, target_tenant_id,
		     correlation_id, http_route, http_method, reason,
		     started_at_utc, is_god_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, id, sessionID, op, tenantID,
		e.CorrelationID, e.HTTPRoute, e.HTTPMethod, e.Reason,
		e.StartedAtUTC, e.IsGodMode)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// ImpersonationAuditWriterNoop is a no-op Write for tests and pre-migration bootstrap.
type ImpersonationAuditWriterNoop struct{}

// Write satisfies [ImpersonationAuditWriter].
func (ImpersonationAuditWriterNoop) Write(_ context.Context, _ ImpersonationAuditEntry) error {
	return nil
}

var (
	_ ImpersonationAuditWriter = (*ImpersonationAuditWriterPG)(nil)
	_ ImpersonationAuditWriter = ImpersonationAuditWriterNoop{}
)
