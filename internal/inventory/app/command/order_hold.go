package command

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// lockedOrderHold is the ONE routine that decides how much an order still holds, shared by
// the release (cancel) and the shipment (dispatch). It must run inside a transaction.
//
// The hold is a sum over the movement ledger, and the sum is only stable while the batch rows
// it moves are locked: a release or shipment of the same order drains the hold under the
// lock of those same batch rows. So the routine
//
//  1. reads which batches the order holds units on (a first look, used only to know which
//     rows to lock),
//  2. locks those batch rows in batch-id order (two orders that hold the same batches in
//     opposite line order then queue instead of deadlocking), and
//  3. reads the outstanding hold again, now under the locks.
//
// A second worker that drained the hold while this one waited for the locks has committed by
// the time the lock is granted, so the second read sees zero and nothing is drained twice.
// Reading the hold BEFORE the transaction, as the consume and release did, let two workers
// both act on the same snapshot and wipe a hold that another order shares (review G2e).
//
// The result is ordered by batch id: the order the rows were locked in.
func lockedOrderHold(ctx context.Context, batches batch.Repository, ledger ReservationLedger, tenantID tenant.ID, orderID string) ([]stockmovement.OutstandingReservation, error) {
	first, err := ledger.ListOutstandingReservationsForOrder(ctx, tenantID, orderID)
	if err != nil {
		return nil, fmt.Errorf("load outstanding: %w", err)
	}
	if len(first) == 0 {
		return nil, nil
	}
	ids := make([]batch.ID, 0, len(first))
	for _, o := range first {
		ids = append(ids, o.BatchID)
	}
	slices.Sort(ids)
	for _, id := range ids {
		// shouldPersist=false: the repository takes the row lock (SELECT ... FOR UPDATE) and
		// keeps it until the transaction ends; nothing is written.
		if err := batches.UpdateByID(ctx, tenantID, id, func(*batch.Batch) (bool, error) { return false, nil }); err != nil {
			return nil, fmt.Errorf("lock batch %s: %w", id, err)
		}
	}
	locked, err := ledger.ListOutstandingReservationsForOrder(ctx, tenantID, orderID)
	if err != nil {
		return nil, fmt.Errorf("load outstanding under lock: %w", err)
	}
	slices.SortFunc(locked, func(a, b stockmovement.OutstandingReservation) int { return cmp.Compare(a.BatchID, b.BatchID) })
	return locked, nil
}
