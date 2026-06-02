//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` bound execution.
//
// SQL-contract coverage (ADR 0062, TDL Test Pyramid):
//   - Add → GetByID / GetByOrderID round-trip under RLS.
//   - UpdateByID status transition persists the mutable lifecycle columns.
//   - UNIQUE(tenant_id, order_id) 23505 → ErrAlreadyExistsForOrder.
//
// arch-test:parallel-safe — every test mints a FRESH tenant_id and the table
// is RLS-scoped, so tests are isolated by tenant (the canon RLS-parallel
// pattern; no TruncateAll, no process-global mutation, own testcontainer).

package adapters_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/dispatch/adapters"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func newNote(t *testing.T, tenantID tenant.ID, orderID consignmentnote.OrderID) *consignmentnote.ConsignmentNote {
	t.Helper()
	cn, err := consignmentnote.New(consignmentnote.NewInput{
		ID:                    consignmentnote.ID(ids.NewV7().String()),
		TenantID:              tenantID,
		OrderID:               orderID,
		CarrierName:           "BlueDart",
		BoxCount:              3,
		WeightGrams:           4500,
		CreatedByMembershipID: membership.ID(ids.NewV7().String()),
		Now:                   nowUTC(),
	})
	if err != nil {
		t.Fatalf("new note: %v", err)
	}
	return cn
}

func TestConsignmentNoteRepository_AddGetRoundTrip(t *testing.T) {
	t.Parallel()
	pool := dispatchPool(t)
	repo := adapters.NewConsignmentNoteRepository(pool, pg.NewTransactor(pool))

	tenantID := tenant.ID(ids.NewV7().String())
	orderID := consignmentnote.OrderID(ids.NewV7().String())
	cn := newNote(t, tenantID, orderID)
	if err := repo.Add(t.Context(), cn); err != nil {
		t.Fatalf("add: %v", err)
	}

	byID, err := repo.GetByID(t.Context(), tenantID, cn.ID())
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if byID.OrderID() != orderID || byID.Status() != consignmentnote.StatusPending {
		t.Errorf("round-trip mismatch: order=%q status=%q", byID.OrderID(), byID.Status())
	}
	byOrder, err := repo.GetByOrderID(t.Context(), tenantID, orderID)
	if err != nil {
		t.Fatalf("get by order: %v", err)
	}
	if byOrder.ID() != cn.ID() {
		t.Errorf("by-order id mismatch: %q != %q", byOrder.ID(), cn.ID())
	}
}

func TestConsignmentNoteRepository_StatusTransitionPersists(t *testing.T) {
	t.Parallel()
	pool := dispatchPool(t)
	repo := adapters.NewConsignmentNoteRepository(pool, pg.NewTransactor(pool))

	tenantID := tenant.ID(ids.NewV7().String())
	cn := newNote(t, tenantID, consignmentnote.OrderID(ids.NewV7().String()))
	if err := repo.Add(t.Context(), cn); err != nil {
		t.Fatalf("add: %v", err)
	}

	actor := membership.ID(ids.NewV7().String())
	err := repo.UpdateByID(t.Context(), tenantID, cn.ID(), func(c *consignmentnote.ConsignmentNote) (bool, error) {
		return true, c.MarkDispatched("BD-DOCKET-123", actor, nowUTC())
	})
	if err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	reloaded, err := repo.GetByID(t.Context(), tenantID, cn.ID())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status() != consignmentnote.StatusDispatched {
		t.Errorf("status = %q, want dispatched", reloaded.Status())
	}
	if reloaded.DocketNumber() != "BD-DOCKET-123" {
		t.Errorf("docket = %q, want BD-DOCKET-123", reloaded.DocketNumber())
	}
	if reloaded.DispatchedAt() == nil {
		t.Error("dispatched_at not persisted")
	}
}

func TestConsignmentNoteRepository_DuplicateOrder_Rejected(t *testing.T) {
	t.Parallel()
	pool := dispatchPool(t)
	repo := adapters.NewConsignmentNoteRepository(pool, pg.NewTransactor(pool))

	tenantID := tenant.ID(ids.NewV7().String())
	orderID := consignmentnote.OrderID(ids.NewV7().String())
	if err := repo.Add(t.Context(), newNote(t, tenantID, orderID)); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// Second note for the SAME (tenant, order) → UNIQUE 23505.
	err := repo.Add(t.Context(), newNote(t, tenantID, orderID))
	if !errors.Is(err, consignmentnote.ErrAlreadyExistsForOrder) {
		t.Fatalf("expected ErrAlreadyExistsForOrder, got %v", err)
	}
}

func TestConsignmentNoteRepository_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	pool := dispatchPool(t)
	repo := adapters.NewConsignmentNoteRepository(pool, pg.NewTransactor(pool))
	_, err := repo.GetByID(t.Context(), tenant.ID(ids.NewV7().String()), consignmentnote.ID(ids.NewV7().String()))
	if !errors.Is(err, consignmentnote.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
