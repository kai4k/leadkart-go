package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// TestNewHardDeleteTenantHandler_PanicsOnNilDeps locks the wiring
// contract: missing repository or memberships dep MUST panic at
// composition time, never silently nil-pointer at request time.
// Per CLAUDE.md "Constructor patterns" — panic at init = sentry,
// silent nil = production bug.
func TestNewHardDeleteTenantHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	t.Run("nil tenants", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil tenants repo")
			}
		}()
		_ = command.NewHardDeleteTenantHandler(nil, newFakeMembershipRepo(), func() time.Time { return testNow })
	})

	t.Run("nil memberships", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil memberships repo")
			}
		}()
		_ = command.NewHardDeleteTenantHandler(newFakeTenantRepo(), nil, func() time.Time { return testNow })
	})
}

// TestHardDeleteTenant_RejectsZeroTenantID exercises the input-shape
// guard before any repository is touched. Companion to the
// integration tests in flow_integration_test.go which drive the
// happy path against a real testcontainers DB.
func TestHardDeleteTenant_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	members := newFakeMembershipRepo()
	h := command.NewHardDeleteTenantHandler(tenants, members, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.HardDeleteTenantCommand{TenantID: tenant.ID("")})
	if err == nil {
		t.Fatal("expected error for zero tenant id, got nil")
	}
}

// TestHardDeleteTenant_NotFound_ReturnsErrTenantNotFound proves the
// not-found sentinel is surfaced via errors.Is for the HTTP layer
// to map to 404 (NOT a wrapped fmt.Errorf — that would lose the
// chain per Russ Cox "Working with Errors in Go 1.13").
func TestHardDeleteTenant_NotFound_ReturnsErrTenantNotFound(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	members := newFakeMembershipRepo()
	h := command.NewHardDeleteTenantHandler(tenants, members, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.HardDeleteTenantCommand{
		TenantID: tenant.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}

// TestHardDeleteTenant_RowDeletedAfterAggregateTransition exercises
// the happy path: tenant in PendingDeletion → HardDelete aggregate
// transition records DeletedEvent → repository row is physically
// deleted. Order matters per the handler comment block:
// audit-event BEFORE row-delete.
func TestHardDeleteTenant_RowDeletedAfterAggregateTransition(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	members := newFakeMembershipRepo()

	tn := newTenant(t)
	// MarkForDeletion is only legal on an Activated tenant; the domain
	// rejects the unactivated → PendingDeletion transition (use
	// HardDelete directly per [Tenant.MarkForDeletion]).
	if err := tn.Activate(testNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := tn.MarkForDeletion("operator-requested-exit", testNow); err != nil {
		t.Fatalf("MarkForDeletion: %v", err)
	}
	tn.PullEvents()
	_ = tenants.Add(t.Context(), tn)

	h := command.NewHardDeleteTenantHandler(tenants, members, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.HardDeleteTenantCommand{TenantID: tn.ID()}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok := tenants.tenants[tn.ID()]; ok {
		t.Error("expected tenant row hard-deleted from repo, still present")
	}
}
