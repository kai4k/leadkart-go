package order_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// advanceTo drives a fresh order to the named state and drains its events.
func advanceTo(t *testing.T, target order.State) *order.Order {
	t.Helper()
	o, err := order.New(sampleNewInput(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	actor := membership.ID(ids.NewV7().String())
	now := fixedNow()
	steps := []struct {
		state order.State
		fn    func() error
	}{
		{order.StateTokenPaid, func() error { return o.RecordTokenPayment(actor, now) }},
		{order.StateConfirmed, func() error { return o.Confirm(actor, now) }},
		{order.StatePacked, func() error { return o.MarkPacked(actor, now) }},
		{order.StateInvoiced, func() error { return o.AttachInvoice("inv-1", actor, now) }},
		{order.StateDispatched, func() error {
			if err := o.AttachConsignment("cn-1"); err != nil {
				return err
			}
			return o.MarkDispatched(actor, now)
		}},
		{order.StateDelivered, func() error { return o.MarkDelivered(actor, now) }},
		{order.StateComplete, func() error { return o.MarkComplete(actor, now) }},
	}
	for _, s := range steps {
		if o.State() == target {
			break
		}
		if err := s.fn(); err != nil {
			t.Fatalf("advance to %s via %s: %v", target, s.state, err)
		}
	}
	o.PullEvents()
	return o
}

// TestOrder_AttachConsignment_ReplayTolerant pins the saga's at-least-once
// contract: re-attaching the SAME consignment is a silent no-op at every later
// state (dispatched, delivered, complete) — no error, no state change, no event.
func TestOrder_AttachConsignment_ReplayTolerant(t *testing.T) {
	t.Parallel()
	for _, state := range []order.State{order.StateInvoiced, order.StateDispatched, order.StateDelivered, order.StateComplete} {
		o := advanceTo(t, state)
		if err := o.AttachConsignment("cn-1"); err != nil {
			t.Fatalf("replay attach at %s: %v", state, err)
		}
		if o.State() != state {
			t.Fatalf("replay attach at %s changed state to %s", state, o.State())
		}
		if n := len(o.PullEvents()); n != 0 {
			t.Fatalf("replay attach at %s emitted %d events, want 0", state, n)
		}
	}
}

// TestOrder_AttachConsignment_RejectsDifferentConsignment pins the one-note-
// per-order invariant: a SECOND consignment ID is a producer bug → ErrInvalid,
// never a silent replacement.
func TestOrder_AttachConsignment_RejectsDifferentConsignment(t *testing.T) {
	t.Parallel()
	o := advanceTo(t, order.StateDispatched)
	err := o.AttachConsignment("cn-OTHER")
	if !errors.Is(err, order.ErrInvalid) {
		t.Fatalf("attach different consignment: err=%v want ErrInvalid", err)
	}
	if o.ConsignmentNoteID() != "cn-1" {
		t.Fatalf("consignment overwritten to %q", o.ConsignmentNoteID())
	}
}

// TestOrder_MarkDelivered_ReplayTolerant pins delivery replay: once
// deliveredAt is stamped, replays no-op even after the order completes
// (terminal) — at-least-once redelivery must never error.
func TestOrder_MarkDelivered_ReplayTolerant(t *testing.T) {
	t.Parallel()
	actor := membership.ID(ids.NewV7().String())
	later := fixedNow().Add(24 * time.Hour)

	for _, state := range []order.State{order.StateDelivered, order.StateComplete} {
		o := advanceTo(t, state)
		before := *o.DeliveredAt()
		if err := o.MarkDelivered(actor, later); err != nil {
			t.Fatalf("replay deliver at %s: %v", state, err)
		}
		if o.State() != state {
			t.Fatalf("replay deliver at %s changed state to %s", state, o.State())
		}
		if !o.DeliveredAt().Equal(before) {
			t.Fatalf("replay deliver at %s moved deliveredAt %v → %v", state, before, *o.DeliveredAt())
		}
		if n := len(o.PullEvents()); n != 0 {
			t.Fatalf("replay deliver at %s emitted %d events, want 0", state, n)
		}
	}
}

// TestOrder_AttachConsignment_OnlyLinksTheNote pins review G2g: creating the consignment
// note links it to the order and nothing else. The order stays invoiced (the goods are
// still in the warehouse), no event is emitted and dispatchedAt stays unset.
func TestOrder_AttachConsignment_OnlyLinksTheNote(t *testing.T) {
	t.Parallel()
	o := advanceTo(t, order.StateInvoiced)
	if err := o.AttachConsignment("cn-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if o.State() != order.StateInvoiced {
		t.Fatalf("attach moved the order to %s", o.State())
	}
	if o.ConsignmentNoteID() != "cn-1" {
		t.Fatalf("consignment = %q", o.ConsignmentNoteID())
	}
	if o.DispatchedAt() != nil {
		t.Fatal("attach stamped dispatchedAt")
	}
	if n := len(o.PullEvents()); n != 0 {
		t.Fatalf("attach emitted %d events, want 0", n)
	}
}

// TestOrder_MarkDispatched_OnlyFromInvoiced pins the one edge into dispatched: the order
// leaves invoiced when the goods leave. Any other state is an invalid transition, and a
// dispatched order is not dispatched a second time.
func TestOrder_MarkDispatched_OnlyFromInvoiced(t *testing.T) {
	t.Parallel()
	actor := membership.ID(ids.NewV7().String())
	now := fixedNow()

	o := advanceTo(t, order.StateInvoiced)
	if err := o.MarkDispatched(actor, now); err != nil {
		t.Fatalf("dispatch invoiced: %v", err)
	}
	if o.State() != order.StateDispatched || o.DispatchedAt() == nil || !o.DispatchedAt().Equal(now) {
		t.Fatalf("state=%s dispatchedAt=%v", o.State(), o.DispatchedAt())
	}
	if n := len(o.PullEvents()); n != 1 {
		t.Fatalf("dispatch emitted %d events, want 1", n)
	}

	for _, state := range []order.State{order.StatePacked, order.StateDispatched, order.StateDelivered} {
		o := advanceTo(t, state)
		if err := o.MarkDispatched(actor, now); !errors.Is(err, order.ErrInvalidTransition) {
			t.Errorf("dispatch from %s: err=%v want ErrInvalidTransition", state, err)
		}
	}
	c := advanceTo(t, order.StateInvoiced)
	if err := c.Cancel("changed mind", actor, now); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkDispatched(actor, now); !errors.Is(err, order.ErrInvalidTransition) {
		t.Errorf("dispatch a cancelled order: err=%v want ErrInvalidTransition", err)
	}
}
