package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
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
		_ = command.NewHardDeleteTenantHandler(nil, newFakeMembershipRepo(), fakeUoW{}, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})

	t.Run("nil memberships", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil memberships repo")
			}
		}()
		_ = command.NewHardDeleteTenantHandler(newFakeTenantRepo(), nil, fakeUoW{}, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
}

// TestHardDeleteTenant_RejectsZeroTenantID — input guard before any repo call.
func TestHardDeleteTenant_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	members := newFakeMembershipRepo()
	h := command.NewHardDeleteTenantHandler(tenants, members, fakeUoW{}, func() time.Time { return testNow })

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
	h := command.NewHardDeleteTenantHandler(tenants, members, fakeUoW{}, func() time.Time { return testNow })

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

	h := command.NewHardDeleteTenantHandler(tenants, members, fakeUoW{}, func() time.Time { return testNow })
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

	h := command.NewHardDeleteTenantHandler(tenants, members, fakeUoW{}, func() time.Time { return testNow })
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
// SQL DELETE failed after the in-memory aggregate transitioned: the handler
// surfaces a wrapped error without panicking. (The in-memory fake has no
// rollback; that the real transaction undoes the transition is asserted by
// TestHardDeleteTenant_DependentsFailTheWholeTransaction and the e2e test.)
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

	h := command.NewHardDeleteTenantHandler(repo, members, fakeUoW{}, func() time.Time { return testNow })

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

// scopeRecordingUoW records the scope a handler opens and whether fn failed (a
// real transaction rolls back on that), and marks the ctx it hands to fn so the
// test can see which repository calls ran inside it.
type scopeRecordingUoW struct {
	scopes []pg.TxScope
	failed bool
}

type inTxKey struct{}

func (u *scopeRecordingUoW) WithinTx(ctx context.Context, scope pg.TxScope, fn func(context.Context) error) error {
	u.scopes = append(u.scopes, scope)
	err := fn(context.WithValue(ctx, inTxKey{}, true))
	u.failed = err != nil
	return err
}

// txProbeTenantRepo notes whether UpdateByID and HardDeleteRow ran inside the
// unit of work, and can fail the row delete with a given error.
type txProbeTenantRepo struct {
	*tenanttest.FakeRepository
	updateInTx, deleteInTx bool
	deleteErr              error
}

func (r *txProbeTenantRepo) UpdateByID(ctx context.Context, id tenant.ID, fn func(*tenant.Tenant) (bool, error)) error {
	r.updateInTx, _ = ctx.Value(inTxKey{}).(bool)
	return r.FakeRepository.UpdateByID(ctx, id, fn)
}

func (r *txProbeTenantRepo) HardDeleteRow(ctx context.Context, id tenant.ID) error {
	r.deleteInTx, _ = ctx.Value(inTxKey{}).(bool)
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.FakeRepository.HardDeleteRow(ctx, id)
}

func pendingDeletionTenant(t *testing.T, repo *tenanttest.FakeRepository) *tenant.Tenant {
	t.Helper()
	tn := newTenant(t)
	if err := tn.Activate(testNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := tn.MarkForDeletion("operator: test", testNow); err != nil {
		t.Fatalf("MarkForDeletion: %v", err)
	}
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup
	return tn
}

// The transition and the row delete are ONE transaction: both run inside a
// single platform-scoped unit of work.
func TestHardDeleteTenant_TransitionAndRowDeleteShareOnePlatformTransaction(t *testing.T) {
	t.Parallel()
	repo := &txProbeTenantRepo{FakeRepository: newFakeTenantRepo()}
	tn := pendingDeletionTenant(t, repo.FakeRepository)
	uow := &scopeRecordingUoW{}
	h := command.NewHardDeleteTenantHandler(repo, newFakeMembershipRepo(), uow, func() time.Time { return testNow })

	if err := h.Handle(t.Context(), command.HardDeleteTenantCommand{TenantID: tn.ID()}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != pg.TxScopePlatform {
		t.Fatalf("unit of work scopes = %v, want exactly one platform-scoped transaction", uow.scopes)
	}
	if !repo.updateInTx || !repo.deleteInTx {
		t.Errorf("inside the transaction: transition=%v row delete=%v, want both true", repo.updateInTx, repo.deleteInTx)
	}
}

// A tenant that still has dependents: the error is matchable (the HTTP layer
// answers 409) and the unit of work saw a failure, so the real transaction
// rolls the transition and its outbox row back.
func TestHardDeleteTenant_DependentsFailTheWholeTransaction(t *testing.T) {
	t.Parallel()
	repo := &txProbeTenantRepo{FakeRepository: newFakeTenantRepo(), deleteErr: tenant.ErrHasDependents}
	tn := pendingDeletionTenant(t, repo.FakeRepository)
	uow := &scopeRecordingUoW{}
	h := command.NewHardDeleteTenantHandler(repo, newFakeMembershipRepo(), uow, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.HardDeleteTenantCommand{TenantID: tn.ID()})
	if !errors.Is(err, tenant.ErrHasDependents) {
		t.Fatalf("err = %v, want it to match tenant.ErrHasDependents", err)
	}
	if !uow.failed {
		t.Error("the unit of work did not see the failure: the transition would be committed")
	}
}

func TestNewHardDeleteTenantHandler_PanicsOnNilUnitOfWork(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil unit of work")
		}
	}()
	_ = command.NewHardDeleteTenantHandler(newFakeTenantRepo(), newFakeMembershipRepo(), nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
}
