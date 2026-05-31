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

// TestNewHardDeleteTenantHandler_PanicsOnNilDeps — a nil tenants or memberships
// dep panics at composition time.
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

// TestHardDeleteTenant_RejectsZeroTenantID — input guard before any repo call.
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

// TestHardDeleteTenant_NotFound_ReturnsErrTenantNotFound — the sentinel
// surfaces via errors.Is (404), not a wrapped error that loses the chain.
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

// TestHardDeleteTenant_RowDeletedAfterAggregateTransition — PendingDeletion →
// HardDelete records DeletedEvent, then the row is physically deleted
// (audit-event before row-delete).
func TestHardDeleteTenant_RowDeletedAfterAggregateTransition(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	members := newFakeMembershipRepo()

	tn := newTenant(t)
	// MarkForDeletion is legal only on an Activated tenant.
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

// TestHardDeleteTenant_PlatformTenant_RejectsBeforeRowTouch — the guard
// consults HasActiveSuperAdmin before touching any row: a SuperAdmin holder
// yields ErrPlatformTenantUndeletable with the row + state intact.
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
	if _, getErr := tenants.GetByID(t.Context(), tn.ID()); getErr != nil {
		t.Errorf("expected tenant row STILL present after guard rejection, GetByID err = %v", getErr)
	}
	if tn.Status() == tenant.StatusDeleted {
		t.Error("expected aggregate state UNCHANGED after guard rejection")
	}
}

// TestHardDeleteTenant_RowDeleteFails_AfterAggregateTransitionSucceeds — the
// aggregate transitioned but the SQL DELETE failed: the handler surfaces a
// wrapped error without panicking (the saga reconciles the inconsistency).
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

	// UpdateByID delegates (transition fires); HardDeleteRow errors.
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
	if tn.Status() != tenant.StatusDeleted {
		t.Errorf("Status = %v, want Deleted (aggregate transition runs BEFORE row-delete)", tn.Status())
	}
}

// failingHardDeleteTenantRepo overrides only HardDeleteRow; UpdateByID still
// delegates so the aggregate transition is observable.
type failingHardDeleteTenantRepo struct {
	*tenanttest.FakeRepository
	hardDeleteErr error
}

func (r *failingHardDeleteTenantRepo) HardDeleteRow(_ context.Context, _ tenant.ID) error {
	return r.hardDeleteErr
}
