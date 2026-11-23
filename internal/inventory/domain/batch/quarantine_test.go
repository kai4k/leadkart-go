package batch_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
)

func stocked(t *testing.T, qty int64) *batch.Batch {
	t.Helper()
	b := freshBatch(t)
	if err := b.ApplyMovement(batch.MovementInbound, qty, fixedNow, fixedNow); err != nil {
		t.Fatalf("inbound: %v", err)
	}
	return b
}

func TestNew_ExpiryMustBeTheLastDayOfAMonth(t *testing.T) {
	t.Parallel()
	bid, pid, tid, actor := freshIDs(t)
	spec := validBatchSpec()
	spec.ExpiryDate = time.Date(2028, 1, 30, 0, 0, 0, 0, time.UTC)
	if _, err := batch.New(bid, pid, tid, actor, spec, fixedNow); !errors.Is(err, batch.ErrInvalid) {
		t.Fatalf("a mid-month expiry must be refused: %v", err)
	}
	spec.ExpiryDate = time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC) // leap February
	if _, err := batch.New(bid, pid, tid, actor, spec, fixedNow); err != nil {
		t.Fatalf("29 Feb 2028 is a month end: %v", err)
	}
}

func TestQuarantine_SetsFreeUnitsApartAndTheyAreNeverAvailable(t *testing.T) {
	t.Parallel()
	b := stocked(t, 100)
	if err := b.ApplyMovement(batch.MovementReservation, 30, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	// 70 are free; 80 is more than free.
	if err := b.ApplyMovement(batch.MovementQuarantine, 80, fixedNow, fixedNow); !errors.Is(err, batch.ErrInsufficientFree) {
		t.Fatalf("quarantining reserved units: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementQuarantine, 20, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	if b.QuantityOnHand() != 100 || b.QuantityQuarantined() != 20 || b.AvailableQuantity() != 50 {
		t.Fatalf("on-hand %d quarantined %d available %d", b.QuantityOnHand(), b.QuantityQuarantined(), b.AvailableQuantity())
	}
	// Quarantined units cannot be reserved, shipped or adjusted away.
	if err := b.ApplyMovement(batch.MovementReservation, 51, fixedNow, fixedNow); !errors.Is(err, batch.ErrInsufficientAvailable) {
		t.Fatalf("reserving quarantined units: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementOutbound, 51, fixedNow, fixedNow); !errors.Is(err, batch.ErrInsufficientStock) {
		t.Fatalf("shipping quarantined units: %v", err)
	}
	// An adjustment down cannot drop on-hand below the held (reserved + quarantined) units: 100 - 60 = 40 < 50.
	if err := b.ApplyMovement(batch.MovementAdjustment, -60, fixedNow, fixedNow); !errors.Is(err, batch.ErrInsufficientStock) {
		t.Fatalf("adjusting away held units: %v", err)
	}
}

func TestQuarantine_FiveBucketsAlwaysSumToOnHand(t *testing.T) {
	t.Parallel()
	b := stocked(t, 100)
	if err := b.ApplyMovement(batch.MovementReservation, 30, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyMovement(batch.MovementQuarantine, 20, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	for _, sellable := range []bool{true, false} {
		bk := b.Buckets(sellable)
		if bk.OnHand != bk.Reserved+bk.Available+bk.Unsellable+bk.Quarantined {
			t.Fatalf("sellable=%v: buckets %+v do not sum to on-hand", sellable, bk)
		}
		if sellable && (bk.Available != 50 || bk.Unsellable != 0) || !sellable && (bk.Available != 0 || bk.Unsellable != 50) {
			t.Fatalf("sellable=%v: %+v", sellable, bk)
		}
	}
}

func TestWriteOff_RemovesQuarantinedUnitsAndKeepsTheRow(t *testing.T) {
	t.Parallel()
	b := stocked(t, 50)
	if err := b.ApplyMovement(batch.MovementWriteOff, 5, fixedNow, fixedNow); !errors.Is(err, batch.ErrInsufficientQuarantined) {
		t.Fatalf("writing off units that are not set apart: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementQuarantine, 10, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyMovement(batch.MovementWriteOff, 4, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	if b.QuantityOnHand() != 46 || b.QuantityQuarantined() != 6 || b.IsDeleted() {
		t.Fatalf("on-hand %d quarantined %d deleted %v: a write-off is a state change, never a delete", b.QuantityOnHand(), b.QuantityQuarantined(), b.IsDeleted())
	}
	if !batch.MovementWriteOff.IsMutating() {
		t.Fatal("a write-off changes on-hand")
	}
}

// Stock past its PRINTED expiry can only stay apart, go back or be written off:
// it cannot be released back to free stock or received into the batch.
func TestPastPrintedExpiry_OnlyQuarantineAndWriteOffRemain(t *testing.T) {
	t.Parallel()
	b := stocked(t, 20)
	if err := b.ApplyMovement(batch.MovementQuarantine, 8, fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	// Business day after the printed month (batch expiry 31 Jan 2028).
	after := time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := b.ApplyMovement(batch.MovementReleaseQuarantine, 8, after, after); !errors.Is(err, batch.ErrExpired) {
		t.Fatalf("releasing quarantine past the printed expiry: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementInbound, 5, after, after); !errors.Is(err, batch.ErrExpired) {
		t.Fatalf("receiving into an expired batch: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementQuarantine, 12, after, after); err != nil {
		t.Fatalf("setting expired stock apart must stay possible: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementWriteOff, 20, after, after); err != nil {
		t.Fatalf("writing expired stock off must stay possible: %v", err)
	}
	// On the last day of the printed month everything still works.
	b2 := stocked(t, 5)
	lastDay := time.Date(2028, 1, 31, 23, 59, 0, 0, time.UTC)
	if err := b2.ApplyMovement(batch.MovementInbound, 1, lastDay, lastDay); err != nil {
		t.Fatalf("inbound on the last day of the printed month: %v", err)
	}
}
