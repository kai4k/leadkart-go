package batch_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// fixedNow is the deterministic timestamp every batch domain test
// passes to factories + mutators per the clock-injection refactor.
// Chosen to be BEFORE the test fixture's exp = 2028-01-01 so
// IsExpired(fixedNow) is false on a happy-path batch.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func freshIDs(t *testing.T) (batch.ID, product.ID, tenant.ID, membership.ID) {
	t.Helper()
	return batch.ID(ids.NewV7().String()),
		product.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String())
}

func farFutureDates() (mfg time.Time, exp time.Time) {
	mfg = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp = time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	return
}

func validBatchSpec() batch.Spec {
	mfg, exp := farFutureDates()
	return batch.Spec{
		BatchNumber:                "LOT-001",
		ManufactureDate:            mfg,
		ExpiryDate:                 exp,
		ManufacturerName:           "Acme Pharma",
		ManufacturingLicenceNumber: "ML-12345",
		MRPPaise:                   25000,
		PurchasePricePaise:         18000,
	}
}

func freshBatch(t *testing.T) *batch.Batch {
	t.Helper()
	bid, pid, tid, actor := freshIDs(t)
	b, err := batch.New(bid, pid, tid, actor, validBatchSpec(), fixedNow)
	if err != nil {
		t.Fatalf("batch.New: %v", err)
	}
	_ = b.PullEvents()
	return b
}

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	bid, pid, tid, actor := freshIDs(t)
	b, err := batch.New(bid, pid, tid, actor, validBatchSpec(), fixedNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.ID() != bid || b.ProductID() != pid || b.TenantID() != tid {
		t.Fatalf("ids: %q %q %q", b.ID(), b.ProductID(), b.TenantID())
	}
	if b.BatchNumber() != "LOT-001" {
		t.Fatalf("batch number: %q", b.BatchNumber())
	}
	if b.QuantityOnHand() != 0 {
		t.Fatalf("quantity must default to 0: %d", b.QuantityOnHand())
	}
	if b.Version() != 0 {
		t.Fatalf("version should start at 0: %d", b.Version())
	}
	if b.MRPPaise() != 25000 || b.PurchasePricePaise() != 18000 {
		t.Fatalf("money: %d %d", b.MRPPaise(), b.PurchasePricePaise())
	}
	evs := b.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	added, ok := evs[0].(batch.AddedEvent)
	if !ok {
		t.Fatalf("event type: %T", evs[0])
	}
	if added.ActorID != actor {
		t.Fatalf("actor: got %q want %q", added.ActorID, actor)
	}
}

func TestNew_InvalidInputs(t *testing.T) {
	t.Parallel()
	bid, pid, tid, actor := freshIDs(t)
	mfg, exp := farFutureDates()
	cases := []struct {
		name  string
		mut   func(s *batch.Spec)
		zeroB bool
		zeroP bool
		zeroT bool
		zeroA bool
	}{
		{name: "zero id", zeroB: true},
		{name: "zero product id", zeroP: true},
		{name: "zero tenant id", zeroT: true},
		{name: "zero actor id", zeroA: true},
		{name: "empty batch number", mut: func(s *batch.Spec) { s.BatchNumber = "" }},
		{name: "batch number too long", mut: func(s *batch.Spec) { s.BatchNumber = strings.Repeat("x", 101) }},
		{name: "manufacturer empty", mut: func(s *batch.Spec) { s.ManufacturerName = "" }},
		{name: "manufacturer too long", mut: func(s *batch.Spec) { s.ManufacturerName = strings.Repeat("x", 201) }},
		{name: "manufacturing licence empty", mut: func(s *batch.Spec) { s.ManufacturingLicenceNumber = "" }},
		{name: "manufacturing licence too long", mut: func(s *batch.Spec) { s.ManufacturingLicenceNumber = strings.Repeat("x", 101) }},
		{name: "mrp negative", mut: func(s *batch.Spec) { s.MRPPaise = -1 }},
		{name: "purchase price negative", mut: func(s *batch.Spec) { s.PurchasePricePaise = -1 }},
		{name: "expiry before manufacture", mut: func(s *batch.Spec) {
			s.ManufactureDate = exp
			s.ExpiryDate = mfg
		}},
		{name: "expiry equals manufacture", mut: func(s *batch.Spec) { s.ExpiryDate = s.ManufactureDate }},
		{name: "manufacture zero", mut: func(s *batch.Spec) { s.ManufactureDate = time.Time{} }},
		{name: "expiry zero", mut: func(s *batch.Spec) { s.ExpiryDate = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := validBatchSpec()
			if tc.mut != nil {
				tc.mut(&spec)
			}
			b := bid
			p := pid
			tn := tid
			a := actor
			if tc.zeroB {
				b = batch.ID("")
			}
			if tc.zeroP {
				p = product.ID("")
			}
			if tc.zeroT {
				tn = tenant.ID("")
			}
			if tc.zeroA {
				a = membership.ID("")
			}
			if _, err := batch.New(b, p, tn, a, spec, fixedNow); !errors.Is(err, batch.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestApplyMovement_InboundIncrements(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	if err := b.ApplyMovement(batch.MovementInbound, 100, fixedNow); err != nil {
		t.Fatalf("Inbound: %v", err)
	}
	if b.QuantityOnHand() != 100 {
		t.Fatalf("on-hand: %d", b.QuantityOnHand())
	}
	if b.Version() != 1 {
		t.Fatalf("version: %d", b.Version())
	}
}

func TestApplyMovement_OutboundDecrements(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	if err := b.ApplyMovement(batch.MovementInbound, 100, fixedNow); err != nil {
		t.Fatalf("seed Inbound: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementOutbound, 30, fixedNow); err != nil {
		t.Fatalf("Outbound: %v", err)
	}
	if b.QuantityOnHand() != 70 {
		t.Fatalf("on-hand: %d", b.QuantityOnHand())
	}
	if b.Version() != 2 {
		t.Fatalf("version: %d", b.Version())
	}
}

func TestApplyMovement_OutboundOverdraftRejected(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	if err := b.ApplyMovement(batch.MovementInbound, 50, fixedNow); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := b.ApplyMovement(batch.MovementOutbound, 51, fixedNow)
	if !errors.Is(err, batch.ErrInsufficientStock) {
		t.Fatalf("want ErrInsufficientStock, got %v", err)
	}
	// On-hand + version unchanged after rejection.
	if b.QuantityOnHand() != 50 {
		t.Fatalf("on-hand should be unchanged: %d", b.QuantityOnHand())
	}
	if b.Version() != 1 {
		t.Fatalf("version unchanged: %d", b.Version())
	}
}

func TestApplyMovement_InboundRejectedAfterExpiry(t *testing.T) {
	t.Parallel()
	bid, pid, tid, actor := freshIDs(t)
	// Expiry already in the past for a freshly-constructed batch.
	mfg := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	b, err := batch.New(bid, pid, tid, actor, batch.Spec{
		BatchNumber:                "LOT-EXPIRED",
		ManufactureDate:            mfg,
		ExpiryDate:                 exp,
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   10000,
		PurchasePricePaise:         8000,
	}, fixedNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = b.PullEvents()
	if err := b.ApplyMovement(batch.MovementInbound, 10, fixedNow); !errors.Is(err, batch.ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	// Outbound from an expired batch IS allowed (write-off / disposal).
	if b.QuantityOnHand() != 0 {
		t.Fatalf("unchanged on-hand: %d", b.QuantityOnHand())
	}
}

func TestApplyMovement_AdjustmentIsSigned(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	if err := b.ApplyMovement(batch.MovementInbound, 100, fixedNow); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementAdjustment, -5, fixedNow); err != nil {
		t.Fatalf("neg adj: %v", err)
	}
	if b.QuantityOnHand() != 95 {
		t.Fatalf("on-hand: %d", b.QuantityOnHand())
	}
	if err := b.ApplyMovement(batch.MovementAdjustment, 3, fixedNow); err != nil {
		t.Fatalf("pos adj: %v", err)
	}
	if b.QuantityOnHand() != 98 {
		t.Fatalf("on-hand: %d", b.QuantityOnHand())
	}
}

func TestApplyMovement_ReservationAndReleaseNonMutating(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	if err := b.ApplyMovement(batch.MovementInbound, 100, fixedNow); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementReservation, 25, fixedNow); err != nil {
		t.Fatalf("reservation: %v", err)
	}
	if b.QuantityOnHand() != 100 {
		t.Fatalf("reservation must NOT mutate on-hand: %d", b.QuantityOnHand())
	}
	if err := b.ApplyMovement(batch.MovementRelease, 25, fixedNow); err != nil {
		t.Fatalf("release: %v", err)
	}
	if b.QuantityOnHand() != 100 {
		t.Fatalf("release must NOT mutate on-hand: %d", b.QuantityOnHand())
	}
}

func TestApplyMovement_RejectsAfterSoftDelete(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	actor := membership.ID(ids.NewV7().String())
	if err := b.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if err := b.ApplyMovement(batch.MovementInbound, 1, fixedNow); !errors.Is(err, batch.ErrDeleted) {
		t.Fatalf("want ErrDeleted, got %v", err)
	}
}

func TestApplyMovement_RejectsZeroQuantityForMutatingTypes(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	for _, tp := range []batch.MovementType{batch.MovementInbound, batch.MovementOutbound, batch.MovementAdjustment} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			if err := b.ApplyMovement(tp, 0, fixedNow); !errors.Is(err, batch.ErrInvalid) {
				t.Fatalf("%v: want ErrInvalid, got %v", tp, err)
			}
		})
	}
}

func TestSoftDelete_IsIdempotent(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	actor := membership.ID(ids.NewV7().String())
	if err := b.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("first SoftDelete: %v", err)
	}
	if !b.IsDeleted() {
		t.Fatal("IsDeleted should be true")
	}
	if b.DeletedBy() != actor.String() {
		t.Fatalf("DeletedBy: got %q want %q", b.DeletedBy(), actor.String())
	}
	pulled := b.PullEvents()
	if err := b.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("second SoftDelete: %v", err)
	}
	if len(b.PullEvents()) != 0 {
		t.Fatalf("second SoftDelete should emit no event (pulled-after-first: %d)", len(pulled))
	}
}

func TestSoftDelete_RejectsZeroActor(t *testing.T) {
	t.Parallel()
	b := freshBatch(t)
	if err := b.SoftDelete(membership.ID(""), fixedNow); !errors.Is(err, batch.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestUnmarshalFromDB_RoundTripsState(t *testing.T) {
	t.Parallel()
	bid, pid, tid, _ := freshIDs(t)
	mfg, exp := farFutureDates()
	b := batch.UnmarshalFromDB(batch.Snapshot{
		ID:                         bid,
		ProductID:                  pid,
		TenantID:                   tid,
		BatchNumber:                "X",
		ManufactureDate:            mfg,
		ExpiryDate:                 exp,
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   100,
		PurchasePricePaise:         50,
		QuantityOnHand:             42,
		Version:                    7,
	})
	if b.QuantityOnHand() != 42 || b.Version() != 7 {
		t.Fatalf("round trip: %d v%d", b.QuantityOnHand(), b.Version())
	}
	if len(b.PullEvents()) != 0 {
		t.Fatal("UnmarshalFromDB should NOT emit events")
	}
}
