package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// HardDeleteTenantCommand performs the SQL-level row delete after the
// 30-day grace window expires + every module has acknowledged the
// TenantDeletedIntegrationEvent per data-retention.md "Tenant
// deletion saga". Distinct from MarkTenantForDeletionCommand which
// only enters the grace window.
//
// Tenant.HardDelete() pre-checks: status MUST be PendingDeletion AND
// the grace window MUST have elapsed; we run that gate via UpdateByID
// (records the audit event) THEN delete the row.
type HardDeleteTenantCommand struct {
	TenantID tenant.ID
}

// HardDeleteTenantHandler runs the two-phase delete.
type HardDeleteTenantHandler struct {
	tenants     tenant.Repository
	memberships membership.Repository
}

// NewHardDeleteTenantHandler wires the handler. memberships is used
// to run the platform-tenant deletion guard (refuse hard-delete on
// tenants holding any active SuperAdmin role-holder) BEFORE any row
// is touched. HardDeleteRow lives on [tenant.Repository] because
// grace-window expiry is a domain operation, not adapter-only.
func NewHardDeleteTenantHandler(tenants tenant.Repository, memberships membership.Repository) HardDeleteTenantHandler {
	if tenants == nil {
		panic("command: NewHardDeleteTenantHandler tenants repository required")
	}
	if memberships == nil {
		panic("command: NewHardDeleteTenantHandler memberships repository required")
	}
	return HardDeleteTenantHandler{tenants: tenants, memberships: memberships}
}

// Handle runs the two-phase delete:
//
//  1. Platform-tenant guard — refuse if any active SuperAdmin role
//     exists in this tenant. Per [ErrPlatformTenantUndeletable].
//  2. UpdateByID with Tenant.HardDelete() — aggregate enforces grace
//     window + records the terminal-state event.
//  3. SQL DELETE the row.
//
// Step 3 fires only if step 2 succeeds — a grace-window-not-elapsed
// rejection short-circuits with the audit-friendly aggregate error
// before the row is touched.
func (h HardDeleteTenantHandler) Handle(ctx context.Context, cmd HardDeleteTenantCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("hard_delete_tenant: tenant id required")
	}
	if err := ensureNotPlatformTenant(ctx, h.memberships, cmd.TenantID); err != nil {
		return err
	}
	err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.HardDelete(); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, tenant.ErrNotFound) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("hard_delete_tenant: aggregate transition: %w", err)
	}
	if err := h.tenants.HardDeleteRow(ctx, cmd.TenantID); err != nil {
		return fmt.Errorf("hard_delete_tenant: row delete: %w", err)
	}
	return nil
}
