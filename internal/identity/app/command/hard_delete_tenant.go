package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// HardDeleteTenantCommand performs the SQL-level row delete after the
// 30-day grace window expires + every module has acknowledged the
// TenantDeletedIntegrationEvent per data-retention.md "Tenant
// deletion saga". Distinct from MarkTenantForDeletionCommand which
// only enters the grace window.
//
// Tenant.HardDelete() pre-checks the lifecycle status; the transition (with its
// tenant_deleted outbox row) and the row delete run in ONE platform-scoped
// transaction, so a delete the database refuses (tenant still has dependents)
// leaves the tenant exactly as it was and publishes nothing.
type HardDeleteTenantCommand struct {
	TenantID tenant.ID
}

// HardDeleteTenantHandler runs the two-phase delete.
type HardDeleteTenantHandler struct {
	tenants     tenant.Repository
	memberships membership.Repository
	uow         pg.UnitOfWork
	now         func() time.Time
}

// NewHardDeleteTenantHandler wires the handler. memberships is used
// to run the platform-tenant deletion guard (refuse hard-delete on
// tenants holding any active SuperAdmin role-holder) BEFORE any row
// is touched. HardDeleteRow lives on [tenant.Repository] because
// grace-window expiry is a domain operation, not adapter-only.
//
// uow opens the single transaction both steps share (the repositories join it
// through the context).
//
// `now` is the explicit time source per the clock-injection refactor.
// Nil → time.Now.
func NewHardDeleteTenantHandler(tenants tenant.Repository, memberships membership.Repository, uow pg.UnitOfWork, now func() time.Time) HardDeleteTenantHandler {
	if tenants == nil {
		panic("command: NewHardDeleteTenantHandler tenants repository required")
	}
	if memberships == nil {
		panic("command: NewHardDeleteTenantHandler memberships repository required")
	}
	if uow == nil {
		panic("command: NewHardDeleteTenantHandler unit of work required")
	}
	if now == nil {
		now = time.Now
	}
	return HardDeleteTenantHandler{tenants: tenants, memberships: memberships, uow: uow, now: now}
}

// Handle runs the delete:
//
//  1. Platform-tenant guard: refuse if any active SuperAdmin role
//     exists in this tenant. Per [ErrPlatformTenantUndeletable].
//  2. In ONE platform-scoped transaction: UpdateByID with Tenant.HardDelete()
//     (aggregate enforces the lifecycle and records the terminal event, which
//     lands in the outbox), then the SQL DELETE of the row.
//
// A foreign-key violation on the DELETE ([tenant.ErrHasDependents]) rolls the
// whole transaction back: the tenant keeps its status and no tenant_deleted row
// is written. The error is returned wrapped so callers can match it.
func (h HardDeleteTenantHandler) Handle(ctx context.Context, cmd HardDeleteTenantCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("hard_delete_tenant: tenant id required")
	}
	if err := ensureNotPlatformTenant(ctx, h.memberships, cmd.TenantID); err != nil {
		return err
	}
	now := h.now()
	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		if err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
			if err := t.HardDelete(now); err != nil {
				return false, err
			}
			return true, nil
		}); err != nil {
			if errors.Is(err, tenant.ErrNotFound) {
				return ErrTenantNotFound
			}
			return fmt.Errorf("hard_delete_tenant: aggregate transition: %w", err)
		}
		if err := h.tenants.HardDeleteRow(ctx, cmd.TenantID); err != nil {
			return fmt.Errorf("hard_delete_tenant: row delete: %w", err)
		}
		return nil
	})
	return err
}
