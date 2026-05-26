package command_test


import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// TestNewHardDeleteTenantHandler_PanicsOnNilDeps locks the wiring
// contract: missing repository or memberships dep MUST panic at
// composition time, never silently nil-pointer at request time.
// Per CLAUDE.md "Constructor patterns" — panic at init = sentry,
// silent nil = production bug.
func TestNewHardDeleteTenantHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	t.Run("nil tenants", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil tenants repo")
			}
		}()
		_ = command.NewHardDeleteTenantHandler(nil, newFakeMembershipRepo(), func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})

	t.Run("nil memberships", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil memberships repo")
			}
		}()
		_ = command.NewHardDeleteTenantHandler(newFakeTenantRepo(), nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
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
	_ = tenants.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup

	h := command.NewHardDeleteTenantHandler(tenants, members, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.HardDeleteTenantCommand{TenantID: tn.ID()}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := tenants.GetByID(t.Context(), tn.ID()); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("expected tenant row hard-deleted from repo, GetByID err = %v, want tenant.ErrNotFound", err)
	}
}

// TestHardDeleteTenant_PlatformTenant_RejectsBeforeRowTouch is the
// security-critical guard test: HardDeleteTenant MUST consult
// HasActiveSuperAdmin BEFORE any row is touched. If a SuperAdmin role-
// holder exists in the tenant, the call returns ErrPlatformUndeletable
// + the row stays intact (no aggregate transition, no row delete).
//
// This is the "do not vaporise the platform tenant by accident" gate.
// Per ADR 0045 + multi-tenancy.md "SuperUser god-mode".
func TestHardDeleteTenant_PlatformTenant_RejectsBeforeRowTouch(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	members := platformTenantMembers()

	tn := newTenant(t)
	if err := tn.Activate(testNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := tn.MarkForDeletion("operator: test", testNow); err != nil {
		t.Fatalf("MarkForDeletion: %v", err)
	}
	tn.PullEvents()
	_ = tenants.Add(t.Context(), tn) // arch-test:ignore-err

	h := command.NewHardDeleteTenantHandler(tenants, members, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.HardDeleteTenantCommand{TenantID: tn.ID()})
	if !errors.Is(err, command.ErrPlatformTenantUndeletable) {
		t.Fatalf("err = %v, want ErrPlatformTenantUndeletable", err)
	}
	// Row must still exist — guard short-circuits BEFORE row-delete.
	if _, getErr := tenants.GetByID(t.Context(), tn.ID()); getErr != nil {
		t.Errorf("expected tenant row STILL present after guard rejection, GetByID err = %v", getErr)
	}
	// And the aggregate must not have transitioned to Deleted.
	if tn.Status() == tenant.StatusDeleted {
		t.Error("expected aggregate state UNCHANGED after guard rejection")
	}
}

// TestHardDeleteTenant_RowDeleteFails_AfterAggregateTransitionSucceeds
// exercises the compensation surface: the aggregate already recorded
// the DeletedEvent (state is Deleted in memory) but the SQL DELETE
// failed. Handler MUST surface a wrapped error and MUST NOT panic.
// The aggregate state ending up "ahead" of the row is a known
// inconsistency window that the deletion saga reconciles per
// `data-retention.md` "Tenant deletion saga".
func TestHardDeleteTenant_RowDeleteFails_AfterAggregateTransitionSucceeds(t *testing.T) {
	t.Parallel()
	inner := newFakeTenantRepo()
	tn := newTenant(t)
	if err := tn.Activate(testNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := tn.MarkForDeletion("operator: test", testNow); err != nil {
		t.Fatalf("MarkForDeletion: %v", err)
	}
	tn.PullEvents()
	_ = inner.Add(t.Context(), tn) // arch-test:ignore-err

	// Failing-row-delete wrapper — UpdateByID delegates so the
	// aggregate transition still fires; HardDeleteRow errors.
	repo := &failingHardDeleteTenantRepo{
		FakeRepository: inner,
		hardDeleteErr:  errBoom,
	}
	members := newFakeMembershipRepo()

	h := command.NewHardDeleteTenantHandler(repo, members, func() time.Time { return testNow })

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("HardDelete panicked on row-delete failure: %v (compensating-state assertion: should surface wrapped error, not panic)", r)
		}
	}()
	err := h.Handle(t.Context(), command.HardDeleteTenantCommand{TenantID: tn.ID()})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom (row-delete failure)", err)
	}
	// Aggregate transition fired before the row-delete attempt.
	if tn.Status() != tenant.StatusDeleted {
		t.Errorf("Status = %v, want Deleted (aggregate transition runs BEFORE row-delete)", tn.Status())
	}
}

// failingHardDeleteTenantRepo wraps the shared fake to override only
// HardDeleteRow. The aggregate-transition path (UpdateByID) still goes
// through the fake so we can observe the aggregate state after the
// compensation-window error.
type failingHardDeleteTenantRepo struct {
	*tenanttest.FakeRepository
	hardDeleteErr error
}

func (r *failingHardDeleteTenantRepo) HardDeleteRow(_ context.Context, _ tenant.ID) error {
	return r.hardDeleteErr
}
