package command_test

import (
	"context"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// sequencedLedger answers each read of the outstanding hold with the next scripted answer and
// logs the read into the same event log the locking batch repository writes to, so a test can
// assert the order: read, lock, lock, read.
type sequencedLedger struct {
	log     *eventLog
	answers [][]stockmovement.OutstandingReservation
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (s *sequencedLedger) CountReservationsForOrder(context.Context, tenant.ID, string) (int64, error) {
	return 0, nil
}

func (s *sequencedLedger) ListOutstandingReservationsForOrder(context.Context, tenant.ID, string) ([]stockmovement.OutstandingReservation, error) {
	s.log.add("read")
	if len(s.answers) == 0 {
		return nil, nil
	}
	a := s.answers[0]
	if len(s.answers) > 1 {
		s.answers = s.answers[1:]
	}
	return a, nil
}

// lockingBatchRepo logs every lock-only UpdateByID (shouldPersist=false) before delegating.
type lockingBatchRepo struct {
	batch.Repository
	log *eventLog
}

func (r lockingBatchRepo) UpdateByID(ctx context.Context, tenantID tenant.ID, id batch.ID, fn func(*batch.Batch) (bool, error)) error {
	r.log.add("lock " + string(id))
	return r.Repository.UpdateByID(ctx, tenantID, id, fn)
}

// Release locks the order's batches in batch-id order BEFORE it reads the hold it drains. The
// hold read under the locks is the one acted on: a hold another worker drained while this one
// waited is gone from it (review G2e, two workers).
func TestReleaseOrderStock_LocksBatchesInIDOrderThenReadsTheHold(t *testing.T) {
	t.Parallel()
	log := &eventLog{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-LOCK-1")
	b1 := seedBatchWithStock(t, batches, p, actor, "B1", 100)
	b2 := seedBatchWithStock(t, batches, p, actor, "B2", 100)
	require.NoError(t, b1.ApplyMovement(batch.MovementReservation, 10, fixedNow, fixedNow))
	require.NoError(t, b2.ApplyMovement(batch.MovementReservation, 10, fixedNow, fixedNow))
	lo, hi := b1, b2
	if lo.ID() > hi.ID() {
		lo, hi = hi, lo
	}
	// The order lists its holds in the OPPOSITE of id order.
	listed := []stockmovement.OutstandingReservation{
		{BatchID: hi.ID(), ProductID: p.ID(), Quantity: 10},
		{BatchID: lo.ID(), ProductID: p.ID(), Quantity: 10},
	}
	ledger := &sequencedLedger{log: log, answers: [][]stockmovement.OutstandingReservation{listed, listed}}

	h := command.NewReleaseOrderStockHandler(&fakeUoW{}, lockingBatchRepo{Repository: batches, log: log}, movements, ledger, fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReleaseOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor, Reason: "order cancelled",
	})
	require.NoError(t, err)
	require.True(t, res.Released)
	require.Equal(t, []string{
		"read",
		"lock " + string(lo.ID()), "lock " + string(hi.ID()),
		"read",
		"lock " + string(lo.ID()), "lock " + string(hi.ID()), // the releases themselves, same order
	}, log.events)
}

// A hold another worker drained while this one waited for the batch locks is not drained
// again: the read under the locks is empty, so nothing moves.
func TestReleaseOrderStock_AHoldDrainedWhileWaitingForTheLocksIsNotDrainedTwice(t *testing.T) {
	t.Parallel()
	log := &eventLog{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-LOCK-2")
	b := seedBatchWithStock(t, batches, p, actor, "B1", 100)
	require.NoError(t, b.ApplyMovement(batch.MovementReservation, 10, fixedNow, fixedNow))
	ledger := &sequencedLedger{log: log, answers: [][]stockmovement.OutstandingReservation{
		{{BatchID: b.ID(), ProductID: p.ID(), Quantity: 10}}, // before the locks: still held
		nil, // under the locks: the other worker drained it
	}}

	h := command.NewReleaseOrderStockHandler(&fakeUoW{}, lockingBatchRepo{Repository: batches, log: log}, movements, ledger, fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReleaseOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor, Reason: "order cancelled",
	})
	require.NoError(t, err)
	require.False(t, res.Released)
	require.Equal(t, int64(10), b.QuantityReserved(), "this worker released nothing")
	require.Zero(t, movements.AddCalls)
}

// Review G2g M3: the reserve takes its batch locks in batch-id order too, the order the
// release and the shipment use, so a reserve and a dispatch that share two batches queue
// instead of deadlocking. The order's lines name the batches in the OPPOSITE order.
func TestReserveOrderStock_LocksBatchesInIDOrder(t *testing.T) {
	t.Parallel()
	log := &eventLog{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	tid := newTenantID(t)
	products := newFakeProductRepo()
	p1 := seedProduct(t, products, tid, actor, "SKU-RL-1")
	p2 := seedProduct(t, products, tid, actor, "SKU-RL-2")
	b1 := seedBatchWithStock(t, batches, p1, actor, "B1", 50)
	b2 := seedBatchWithStock(t, batches, p2, actor, "B2", 50)
	lo, hi := b1, b2
	if lo.ID() > hi.ID() {
		lo, hi = hi, lo
	}
	loProduct, hiProduct := p1, p2
	if lo.ID() == b2.ID() {
		loProduct, hiProduct = p2, p1
	}

	h := command.NewReserveOrderStockHandler(&fakeUoW{}, lockingBatchRepo{Repository: batches, log: log}, movements, &fakeReservationLedger{}, &fakeEnqueuer{}, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReserveOrderStockCommand{
		TenantID: tid, OrderID: uuid.NewString(), ActorMembershipID: actor,
		// the line of the HIGHER batch id first
		Lines: []command.ReserveLine{{ProductID: hiProduct.ID(), Quantity: 5}, {ProductID: loProduct.ID(), Quantity: 5}},
	})
	require.NoError(t, err)
	require.True(t, res.Reserved)
	require.Equal(t, []string{"lock " + string(lo.ID()), "lock " + string(hi.ID())}, log.events)
}
