package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
)

func TestLogStockMovementHandler_HappyPath_Inbound(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	movementRepo := newFakeMovementRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "MV-1")
	b := seedBatch(t, batchRepo, p, actor, "LOT-1")
	h := command.NewLogStockMovementHandler(uow, batchRepo, movementRepo, func() time.Time { return fixedNow }, testNewMovementID)

	out, err := h.Handle(t.Context(), command.LogStockMovementCommand{
		BatchID:           b.ID(),
		ActorMembershipID: actor,
		Type:              batch.MovementInbound,
		Quantity:          100,
		Reason:            "initial inbound",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.QuantityOnHandAfter != 100 {
		t.Fatalf("QuantityOnHandAfter: got %d want 100", out.QuantityOnHandAfter)
	}
	if movementRepo.addCalls != 1 {
		t.Fatalf("movementRepo.addCalls: got %d want 1", movementRepo.addCalls)
	}
	if uow.Runs() != 1 {
		t.Fatalf("UoW runs (no contention): got %d want 1", uow.Runs())
	}
}

// Outbound conversion: caller passes positive magnitude; handler stores
// negative quantity in the ledger (Outbound = subtract).
func TestLogStockMovementHandler_OutboundSignsQuantityNegative(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	movementRepo := newFakeMovementRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "MV-OUT")
	b := seedBatch(t, batchRepo, p, actor, "LOT-OUT")
	if err := b.ApplyMovement(batch.MovementInbound, 100, fixedNow); err != nil {
		t.Fatalf("seed inbound: %v", err)
	}
	_ = b.PullEvents()

	h := command.NewLogStockMovementHandler(uow, batchRepo, movementRepo, func() time.Time { return fixedNow }, testNewMovementID)
	_, err := h.Handle(t.Context(), command.LogStockMovementCommand{
		BatchID:           b.ID(),
		ActorMembershipID: actor,
		Type:              batch.MovementOutbound,
		Quantity:          30, // positive magnitude → ledger -30
		Reason:            "dispatch",
	})
	if err != nil {
		t.Fatalf("Handle outbound: %v", err)
	}
	// Pull the just-added movement and confirm signed convention.
	if len(movementRepo.movements) != 1 {
		t.Fatalf("movement count: %d", len(movementRepo.movements))
	}
	for _, m := range movementRepo.movements {
		if m.Quantity() != -30 {
			t.Fatalf("Outbound stored quantity: got %d want -30 (signed ledger convention)", m.Quantity())
		}
	}
}

// Failure 1: invalid type → ErrInvalid (early reject; no UoW entry).
func TestLogStockMovementHandler_InvalidType_ReturnsErrInvalid(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	movementRepo := newFakeMovementRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "MV-BADTYPE")
	b := seedBatch(t, batchRepo, p, actor, "LOT-BT")

	h := command.NewLogStockMovementHandler(uow, batchRepo, movementRepo, func() time.Time { return fixedNow }, testNewMovementID)
	_, err := h.Handle(t.Context(), command.LogStockMovementCommand{
		BatchID:           b.ID(),
		ActorMembershipID: actor,
		Type:              batch.MovementType("garbage"),
		Quantity:          1,
		Reason:            "bad",
	})
	if !errors.Is(err, batch.ErrInvalid) {
		t.Fatalf("err: got %v want ErrInvalid", err)
	}
	if uow.Runs() != 0 {
		t.Fatalf("UoW MUST NOT run on early-reject: got %d", uow.Runs())
	}
}

// Failure 2: zero magnitude → ErrInvalid (early reject).
func TestLogStockMovementHandler_ZeroQuantity_ReturnsErrInvalid(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	movementRepo := newFakeMovementRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "MV-ZERO")
	b := seedBatch(t, batchRepo, p, actor, "LOT-Z")

	h := command.NewLogStockMovementHandler(uow, batchRepo, movementRepo, func() time.Time { return fixedNow }, testNewMovementID)
	_, err := h.Handle(t.Context(), command.LogStockMovementCommand{
		BatchID:           b.ID(),
		ActorMembershipID: actor,
		Type:              batch.MovementInbound,
		Quantity:          0,
		Reason:            "zero",
	})
	if !errors.Is(err, batch.ErrInvalid) {
		t.Fatalf("err: got %v want ErrInvalid", err)
	}
	if uow.Runs() != 0 {
		t.Fatalf("UoW MUST NOT run on zero-qty reject: got %d", uow.Runs())
	}
}

// Failure 3: insufficient stock surfaces directly (no retry — domain
// reject, not optimistic-concurrency).
func TestLogStockMovementHandler_InsufficientStock_NoRetry(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	movementRepo := newFakeMovementRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "MV-LOW")
	b := seedBatch(t, batchRepo, p, actor, "LOT-LOW") // on-hand = 0

	h := command.NewLogStockMovementHandler(uow, batchRepo, movementRepo, func() time.Time { return fixedNow }, testNewMovementID)
	_, err := h.Handle(t.Context(), command.LogStockMovementCommand{
		BatchID:           b.ID(),
		ActorMembershipID: actor,
		Type:              batch.MovementOutbound,
		Quantity:          50,
		Reason:            "underflow",
	})
	if !errors.Is(err, batch.ErrInsufficientStock) {
		t.Fatalf("err: got %v want ErrInsufficientStock", err)
	}
	// UoW ran exactly once — no retry on a domain-side reject.
	if uow.Runs() != 1 {
		t.Fatalf("UoW runs on insufficient-stock (no retry): got %d want 1", uow.Runs())
	}
	if movementRepo.addCalls != 0 {
		t.Fatalf("movement MUST NOT be persisted on reject: addCalls=%d", movementRepo.addCalls)
	}
}

// Negative magnitude rejected early — handler enforces "caller supplies
// magnitude, handler picks the sign".
func TestLogStockMovementHandler_NegativeMagnitude_ReturnsErrInvalid(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	movementRepo := newFakeMovementRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "MV-NEG")
	b := seedBatch(t, batchRepo, p, actor, "LOT-NEG")

	h := command.NewLogStockMovementHandler(uow, batchRepo, movementRepo, func() time.Time { return fixedNow }, testNewMovementID)
	_, err := h.Handle(t.Context(), command.LogStockMovementCommand{
		BatchID:           b.ID(),
		ActorMembershipID: actor,
		Type:              batch.MovementInbound,
		Quantity:          -5,
		Reason:            "wrong sign",
	})
	if !errors.Is(err, batch.ErrInvalid) {
		t.Fatalf("err: got %v want ErrInvalid (negative magnitude)", err)
	}
}
