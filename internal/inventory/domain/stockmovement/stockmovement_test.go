package stockmovement_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

func freshIDs(t *testing.T) (stockmovement.ID, batch.ID, product.ID, tenant.ID, membership.ID) {
	t.Helper()
	return stockmovement.ID(ids.NewV7().String()),
		batch.ID(ids.NewV7().String()),
		product.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String())
}

func TestNew_InboundHappyPath(t *testing.T) {
	t.Parallel()
	mid, bid, pid, tid, actor := freshIDs(t)
	src := "po-123"
	m, err := stockmovement.New(mid, stockmovement.Spec{
		BatchID:             bid,
		ProductID:           pid,
		TenantID:            tid,
		Type:                batch.MovementInbound,
		Quantity:            100,
		QuantityOnHandAfter: 100,
		Reason:              "Initial stock-in",
		ActorMembershipID:   actor,
		SourceReference:     &src,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.ID() != mid || m.BatchID() != bid || m.TenantID() != tid {
		t.Fatalf("ids: %q %q %q", m.ID(), m.BatchID(), m.TenantID())
	}
	if m.Type() != batch.MovementInbound || m.Quantity() != 100 {
		t.Fatalf("type/qty: %v %d", m.Type(), m.Quantity())
	}
	if m.SourceReference() == nil || *m.SourceReference() != "po-123" {
		t.Fatalf("source ref: %v", m.SourceReference())
	}
	if m.QuantityOnHandAfter() != 100 {
		t.Fatalf("on-hand after: %d", m.QuantityOnHandAfter())
	}
	if m.OccurredAt().IsZero() {
		t.Fatal("OccurredAt zero")
	}
	evs := m.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	if _, ok := evs[0].(stockmovement.LoggedEvent); !ok {
		t.Fatalf("event type: %T", evs[0])
	}
}

func TestNew_NilSourceReference_OK(t *testing.T) {
	t.Parallel()
	mid, bid, pid, tid, actor := freshIDs(t)
	m, err := stockmovement.New(mid, stockmovement.Spec{
		BatchID:             bid,
		ProductID:           pid,
		TenantID:            tid,
		Type:                batch.MovementAdjustment,
		Quantity:            -5,
		QuantityOnHandAfter: 95,
		Reason:              "shrinkage",
		ActorMembershipID:   actor,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.SourceReference() != nil {
		t.Fatalf("source ref should be nil: %v", m.SourceReference())
	}
}

func TestNew_InvalidInputs(t *testing.T) {
	t.Parallel()
	mid, bid, pid, tid, actor := freshIDs(t)
	baseline := stockmovement.Spec{
		BatchID:             bid,
		ProductID:           pid,
		TenantID:            tid,
		Type:                batch.MovementInbound,
		Quantity:            100,
		QuantityOnHandAfter: 100,
		Reason:              "test",
		ActorMembershipID:   actor,
	}
	cases := []struct {
		name string
		mut  func(s *stockmovement.Spec)
		zid  bool
	}{
		{name: "zero id", zid: true},
		{name: "zero batch", mut: func(s *stockmovement.Spec) { s.BatchID = batch.ID("") }},
		{name: "zero product", mut: func(s *stockmovement.Spec) { s.ProductID = product.ID("") }},
		{name: "zero tenant", mut: func(s *stockmovement.Spec) { s.TenantID = tenant.ID("") }},
		{name: "zero actor", mut: func(s *stockmovement.Spec) { s.ActorMembershipID = membership.ID("") }},
		{name: "empty reason", mut: func(s *stockmovement.Spec) { s.Reason = "" }},
		{name: "reason too long", mut: func(s *stockmovement.Spec) { s.Reason = strings.Repeat("x", 501) }},
		{name: "unknown type", mut: func(s *stockmovement.Spec) { s.Type = batch.MovementType("frob") }},
		{name: "inbound zero qty", mut: func(s *stockmovement.Spec) { s.Quantity = 0 }},
		{name: "inbound negative qty", mut: func(s *stockmovement.Spec) { s.Quantity = -1 }},
		{name: "outbound zero qty", mut: func(s *stockmovement.Spec) { s.Type = batch.MovementOutbound; s.Quantity = 0 }},
		{name: "outbound positive qty", mut: func(s *stockmovement.Spec) { s.Type = batch.MovementOutbound; s.Quantity = 5 }},
		{name: "adjustment zero qty", mut: func(s *stockmovement.Spec) { s.Type = batch.MovementAdjustment; s.Quantity = 0 }},
		{name: "reservation negative qty", mut: func(s *stockmovement.Spec) { s.Type = batch.MovementReservation; s.Quantity = -1 }},
		{name: "negative on-hand-after", mut: func(s *stockmovement.Spec) { s.QuantityOnHandAfter = -1 }},
		{name: "source ref too long", mut: func(s *stockmovement.Spec) {
			x := strings.Repeat("x", 201)
			s.SourceReference = &x
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := baseline
			if tc.mut != nil {
				tc.mut(&spec)
			}
			id := mid
			if tc.zid {
				id = stockmovement.ID("")
			}
			if _, err := stockmovement.New(id, spec); !errors.Is(err, stockmovement.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNew_OutboundQuantityIsStoredAsSignedNegative(t *testing.T) {
	// Convention check: Outbound is the canonical "minus" movement.
	// Test that the Spec.Quantity convention is "outbound writes a
	// negative number" so the ledger SUM(quantity) = batch.on-hand
	// without per-row type-switching at read time.
	t.Parallel()
	mid, bid, pid, tid, actor := freshIDs(t)
	m, err := stockmovement.New(mid, stockmovement.Spec{
		BatchID:             bid,
		ProductID:           pid,
		TenantID:            tid,
		Type:                batch.MovementOutbound,
		Quantity:            -30,
		QuantityOnHandAfter: 70,
		Reason:              "order fulfilment",
		ActorMembershipID:   actor,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Quantity() != -30 {
		t.Fatalf("quantity: %d", m.Quantity())
	}
}

func TestNew_AdjustmentAcceptsAnyNonZero(t *testing.T) {
	t.Parallel()
	for _, q := range []int64{1, -1, 100, -100} {
		mid, bid, pid, tid, actor := freshIDs(t)
		// QuantityOnHandAfter must be >= 0 so we offset to make it valid.
		base := int64(100)
		_, err := stockmovement.New(mid, stockmovement.Spec{
			BatchID:             bid,
			ProductID:           pid,
			TenantID:            tid,
			Type:                batch.MovementAdjustment,
			Quantity:            q,
			QuantityOnHandAfter: base + q,
			Reason:              "adj",
			ActorMembershipID:   actor,
		})
		if err != nil {
			t.Fatalf("Adjustment qty=%d: %v", q, err)
		}
	}
}

func TestUnmarshalFromDB_DoesNotEmitEvents(t *testing.T) {
	t.Parallel()
	mid, bid, pid, tid, actor := freshIDs(t)
	m := stockmovement.UnmarshalFromDB(stockmovement.Snapshot{
		ID:                  mid,
		BatchID:             bid,
		ProductID:           pid,
		TenantID:            tid,
		Type:                batch.MovementInbound,
		Quantity:            10,
		QuantityOnHandAfter: 10,
		Reason:              "x",
		ActorMembershipID:   actor,
	})
	if len(m.PullEvents()) != 0 {
		t.Fatal("UnmarshalFromDB should NOT emit events")
	}
}
