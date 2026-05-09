package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Errors ---------------------------------------------------------------

// ErrTenantNotFound is the public-facing 404 surface for any tenant
// lifecycle command. Mirrors the Auth0/Okta pattern of collapsing
// "wrong owner" + "doesn't exist" into a single error to defeat
// tenant-id enumeration.
var ErrTenantNotFound = errors.New("tenant: not found")

// ErrPlatformTenantUndeletable is returned when a destructive
// lifecycle command (Suspend / MarkForDeletion / HardDelete) targets
// a tenant that holds an active SuperAdmin role-holder. The platform
// would lose its operator god-mode if accidentally deleted; this guard
// short-circuits before any aggregate transition fires.
//
// Surfaces as HTTP 422 with a clear message — operators must first
// move SuperAdmin role-holders off the tenant (or rotate to a
// different platform tenant) before deletion is permitted.
var ErrPlatformTenantUndeletable = errors.New("tenant: cannot delete a tenant that holds an active SuperAdmin role")

// ensureNotPlatformTenant runs the shared deletion-guard check before
// any destructive lifecycle transition. Returns ErrPlatformTenantUndeletable
// if the supplied tenant has any active SuperAdmin role-holder.
func ensureNotPlatformTenant(ctx context.Context, members membership.Repository, tid tenant.ID) error {
	has, err := members.HasActiveSuperAdmin(ctx, tid)
	if err != nil {
		return fmt.Errorf("platform-tenant guard: %w", err)
	}
	if has {
		return ErrPlatformTenantUndeletable
	}
	return nil
}

// ----- SuspendTenant --------------------------------------------------------

// SuspendTenantCommand suspends a tenant. Reason MUST be non-empty per
// `data-retention.md` audit canon — surfaces in the audit log + the
// emitted [tenant.SuspendedEvent].
type SuspendTenantCommand struct {
	TenantID tenant.ID
	Reason   string
}

// SuspendTenantHandler runs the suspend flow.
type SuspendTenantHandler struct {
	tenants     tenant.Repository
	memberships membership.Repository
}

// NewSuspendTenantHandler wires the handler. memberships is used to
// run the platform-tenant deletion guard before transitioning.
func NewSuspendTenantHandler(tenants tenant.Repository, memberships membership.Repository) SuspendTenantHandler {
	if tenants == nil {
		panic("command: NewSuspendTenantHandler tenants repository required")
	}
	if memberships == nil {
		panic("command: NewSuspendTenantHandler memberships repository required")
	}
	return SuspendTenantHandler{tenants: tenants, memberships: memberships}
}

// Handle dispatches to [Tenant.Suspend]. Refuses tenants holding any
// active SuperAdmin role-holder per [ErrPlatformTenantUndeletable].
func (h SuspendTenantHandler) Handle(ctx context.Context, cmd SuspendTenantCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("suspend_tenant: tenant id required")
	}
	if err := ensureNotPlatformTenant(ctx, h.memberships, cmd.TenantID); err != nil {
		return err
	}
	err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.Suspend(cmd.Reason); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, tenant.ErrNotFound) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("suspend_tenant: %w", err)
	}
	return nil
}

// ----- ActivateTenant -------------------------------------------------------

// ActivateTenantCommand activates a suspended OR pending tenant.
type ActivateTenantCommand struct {
	TenantID tenant.ID
}

// ActivateTenantHandler runs the activate flow.
type ActivateTenantHandler struct {
	tenants tenant.Repository
}

// NewActivateTenantHandler wires the handler.
func NewActivateTenantHandler(tenants tenant.Repository) ActivateTenantHandler {
	if tenants == nil {
		panic("command: NewActivateTenantHandler tenants repository required")
	}
	return ActivateTenantHandler{tenants: tenants}
}

// Handle dispatches to [Tenant.Activate]. Idempotent — already-active
// returns nil without an event per the aggregate's no-op contract.
func (h ActivateTenantHandler) Handle(ctx context.Context, cmd ActivateTenantCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("activate_tenant: tenant id required")
	}
	err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.Activate(); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, tenant.ErrNotFound) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("activate_tenant: %w", err)
	}
	return nil
}

// ----- MarkTenantForDeletion ------------------------------------------------

// MarkTenantForDeletionCommand initiates the 30-day grace deletion
// window per `data-retention.md` "Tenant deletion saga". Reason MUST
// be non-empty (DPDP §12 + SOC2 CC4.1 audit requirement).
type MarkTenantForDeletionCommand struct {
	TenantID tenant.ID
	Reason   string
}

// MarkTenantForDeletionHandler runs the mark-for-deletion flow.
type MarkTenantForDeletionHandler struct {
	tenants     tenant.Repository
	memberships membership.Repository
}

// NewMarkTenantForDeletionHandler wires the handler. memberships is
// used to run the platform-tenant deletion guard before transitioning.
func NewMarkTenantForDeletionHandler(tenants tenant.Repository, memberships membership.Repository) MarkTenantForDeletionHandler {
	if tenants == nil {
		panic("command: NewMarkTenantForDeletionHandler tenants repository required")
	}
	if memberships == nil {
		panic("command: NewMarkTenantForDeletionHandler memberships repository required")
	}
	return MarkTenantForDeletionHandler{tenants: tenants, memberships: memberships}
}

// Handle dispatches to [Tenant.MarkForDeletion]. Aggregate enforces:
// only Active/Suspended → PendingDeletion; idempotent only if reason
// matches the existing schedule's reason. Refuses tenants holding any
// active SuperAdmin role-holder per [ErrPlatformTenantUndeletable].
func (h MarkTenantForDeletionHandler) Handle(ctx context.Context, cmd MarkTenantForDeletionCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("mark_tenant_for_deletion: tenant id required")
	}
	if err := ensureNotPlatformTenant(ctx, h.memberships, cmd.TenantID); err != nil {
		return err
	}
	err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.MarkForDeletion(cmd.Reason); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, tenant.ErrNotFound) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("mark_tenant_for_deletion: %w", err)
	}
	return nil
}

// ----- RestoreTenant --------------------------------------------------------

// RestoreTenantCommand cancels a pending deletion within the 30-day
// grace window. Aggregate transitions PendingDeletion → Active.
type RestoreTenantCommand struct {
	TenantID tenant.ID
}

// RestoreTenantHandler runs the restore flow.
type RestoreTenantHandler struct {
	tenants tenant.Repository
}

// NewRestoreTenantHandler wires the handler.
func NewRestoreTenantHandler(tenants tenant.Repository) RestoreTenantHandler {
	if tenants == nil {
		panic("command: NewRestoreTenantHandler tenants repository required")
	}
	return RestoreTenantHandler{tenants: tenants}
}

// Handle dispatches to [Tenant.RestoreFromDeletion]. Aggregate
// enforces: only PendingDeletion → Active is allowed.
func (h RestoreTenantHandler) Handle(ctx context.Context, cmd RestoreTenantCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("restore_tenant: tenant id required")
	}
	err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.RestoreFromDeletion(); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, tenant.ErrNotFound) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("restore_tenant: %w", err)
	}
	return nil
}
