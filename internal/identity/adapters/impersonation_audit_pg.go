// impersonation_audit_pg.go — persistent audit-log writer for the
// platform-operator impersonation flow.
//
// Per ADR 0051 (Wave 9.1b): types renamed from `impersonation.*` to
// `ImpersonationAudit*` to avoid collision in the flat adapters
// package, where multiple Pg-backed writers coexist. The interface
// lives here for now because v0.2 has no consumer; when Wave 4.1's
// audit-log enrichment subscriber actually wires this up, the
// interface should migrate to `internal/identity/domain/impersonation/`
// per Cheney "accept interfaces, return structs."
//
// Wired in Phase 2+ when the AuditLoggingMiddleware learns to surface
// the X-Impersonation-Session-Id header → operator-correlation row
// via this writer. v0.2 has the table migration shipped (20260507000006)
// but no row insertions yet.

package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// ImpersonationAuditEntry is one persistent activity row written for
// each request the operator runs under an impersonation session.
// Mirrors the .NET `AdminImpersonationAudit` Marten document shape.
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
// Implementations:
//   - [ImpersonationAuditWriterPG]   writes to
//     buildingblocks.admin_impersonation_audit.
//   - [ImpersonationAuditWriterNoop] swallows everything (tests / dev
//     where the audit table isn't migrated).
type ImpersonationAuditWriter interface {
	Write(ctx context.Context, entry ImpersonationAuditEntry) error
}

// ImpersonationAuditWriterPG is the production-path adapter.
type ImpersonationAuditWriterPG struct {
	pool *pgxpool.Pool
}

// NewImpersonationAuditWriterPG wires the writer against a pgxpool.
// The admin_impersonation_audit table is non-RLS — no tenant context
// needed for the INSERT.
func NewImpersonationAuditWriterPG(pool *pgxpool.Pool) *ImpersonationAuditWriterPG {
	if pool == nil {
		panic("impersonation: NewImpersonationAuditWriterPG pool required")
	}
	return &ImpersonationAuditWriterPG{pool: pool}
}

// Write satisfies [ImpersonationAuditWriter].
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
		INSERT INTO buildingblocks.admin_impersonation_audit
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

// ImpersonationAuditWriterNoop swallows every Write. Used in tests +
// bootstrap before the buildingblocks schema is migrated.
type ImpersonationAuditWriterNoop struct{}

// Write satisfies [ImpersonationAuditWriter].
func (ImpersonationAuditWriterNoop) Write(_ context.Context, _ ImpersonationAuditEntry) error {
	return nil
}

var (
	_ ImpersonationAuditWriter = (*ImpersonationAuditWriterPG)(nil)
	_ ImpersonationAuditWriter = ImpersonationAuditWriterNoop{}
)
