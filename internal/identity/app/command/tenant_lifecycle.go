package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Errors ---------------------------------------------------------------

// ErrTenantNotFound is the public-facing 404 surface for any tenant
// lifecycle command. Mirrors the Auth0/Okta pattern of collapsing
// "wrong owner" + "doesn't exist" into a single error to defeat
// tenant-id enumeration.
var ErrTenantNotFound = errors.New("tenant: not found")

// ErrPlatformTenantUndeletable is returned when the suspend command targets
// a tenant that holds an active SuperAdmin role-holder. The platform
// would lose its operator god-mode if accidentally suspended; this guard
// short-circuits before any aggregate transition fires.
//
// Surfaces as HTTP 422 with a clear message — operators must first
// move SuperAdmin role-holders off the tenant (or rotate to a
// different platform tenant) before the suspension is permitted.
var ErrPlatformTenantUndeletable = errors.New("tenant: cannot delete a tenant that holds an active SuperAdmin role")

// ensureNotPlatformTenant runs the platform-tenant guard check before
// the suspend transition. Returns ErrPlatformTenantUndeletable
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
	now         func() time.Time
}

// NewSuspendTenantHandler wires the handler. memberships is used to
// run the platform-tenant guard before transitioning.
// `now` is the explicit time source per the clock-injection refactor.
// Nil → time.Now.
func NewSuspendTenantHandler(tenants tenant.Repository, memberships membership.Repository, now func() time.Time) SuspendTenantHandler {
	if tenants == nil {
		panic("command: NewSuspendTenantHandler tenants repository required")
	}
	if memberships == nil {
		panic("command: NewSuspendTenantHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return SuspendTenantHandler{tenants: tenants, memberships: memberships, now: now}
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
	now := h.now()
	err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.Suspend(cmd.Reason, now); err != nil {
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

// ActivateTenantCommand activates a suspended OR pending tenant. An already-active
// tenant is a harmless no-op (204, no event, nothing changes): registration creates
// companies active, so a client approving a "new" company meets this answer.
type ActivateTenantCommand struct {
	TenantID tenant.ID
}

// ActivateTenantHandler runs the activate flow.
type ActivateTenantHandler struct {
	tenants tenant.Repository
	now     func() time.Time
}

// NewActivateTenantHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewActivateTenantHandler(tenants tenant.Repository, now func() time.Time) ActivateTenantHandler {
	if tenants == nil {
		panic("command: NewActivateTenantHandler tenants repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ActivateTenantHandler{tenants: tenants, now: now}
}

// Handle dispatches to [Tenant.Activate]. Idempotent — already-active
// returns nil without an event per the aggregate's no-op contract.
func (h ActivateTenantHandler) Handle(ctx context.Context, cmd ActivateTenantCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("activate_tenant: tenant id required")
	}
	now := h.now()
	err := h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.Activate(now); err != nil {
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
