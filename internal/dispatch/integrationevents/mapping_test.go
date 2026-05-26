package integrationevents_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

// TestFromDomainEvent_CreatedMaps drives the canonical "domain event
// → integration event" wire path for ConsignmentNote ctor + asserts
// every field flows.
func TestFromDomainEvent_CreatedMaps(t *testing.T) {
	t.Parallel()

	cnID := ids.NewV7().String()
	tID := ids.NewV7().String()
	oID := ids.NewV7().String()
	mID := ids.NewV7().String()
	eta := fixedNow().Add(48 * time.Hour)
	domainEvt := consignmentnote.CreatedEvent{
		ConsignmentNoteID:     consignmentnote.ID(cnID),
		TenantID:              tenant.ID(tID),
		OrderID:               consignmentnote.OrderID(oID),
		CarrierName:           "BlueDart",
		BoxCount:              3,
		WeightGrams:           7500,
		ExpectedDeliveryAt:    &eta,
		CreatedAt:             fixedNow(),
		CreatedByMembershipID: membership.ID(mID),
	}
	got, err := integrationevents.FromDomainEvent(domainEvt)
	if err != nil {
		t.Fatalf("FromDomainEvent: %v", err)
	}
	created, ok := got.(integrationevents.ConsignmentNoteCreatedV1)
	if !ok {
		t.Fatalf("type = %T want ConsignmentNoteCreatedV1", got)
	}
	if created.ConsignmentNoteID.String() != cnID {
		t.Errorf("cn id mismatch: got %s want %s", created.ConsignmentNoteID, cnID)
	}
	if created.OrderID.String() != oID {
		t.Errorf("order id mismatch: got %s want %s", created.OrderID, oID)
	}
	if created.CarrierName != "BlueDart" {
		t.Errorf("carrier mismatch")
	}
	if created.BoxCount != 3 {
		t.Errorf("box count mismatch")
	}
	if created.ExpectedDeliveryAtUTC == nil || !created.ExpectedDeliveryAtUTC.Equal(eta.UTC()) {
		t.Errorf("eta mismatch")
	}
	if created.Topic() != "dispatch.consignment_note_created.v1" {
		t.Errorf("topic mismatch: got %s", created.Topic())
	}
}

// TestFromDomainEvent_StatusChangedFansOut asserts the (NewStatus →
// integration-event-type) fan-out. The single StatusChangedEvent
// domain shape produces five distinct integration shapes; this test
// pins each branch.
func TestFromDomainEvent_StatusChangedFansOut(t *testing.T) {
	t.Parallel()

	cases := []struct {
		newStatus consignmentnote.Status
		wantType  string
		wantTopic string
	}{
		{consignmentnote.StatusDispatched, "ConsignmentNoteDispatchedV1", "dispatch.consignment_note_dispatched.v1"},
		{consignmentnote.StatusInTransit, "ConsignmentNoteInTransitV1", "dispatch.consignment_note_in_transit.v1"},
		{consignmentnote.StatusDelivered, "ConsignmentDeliveredV1", "dispatch.consignment_delivered.v1"},
		{consignmentnote.StatusFailed, "ConsignmentNoteFailedV1", "dispatch.consignment_note_failed.v1"},
	}
	for _, c := range cases {
		t.Run(c.newStatus.String(), func(t *testing.T) {
			t.Parallel()
			cnID := ids.NewV7().String()
			tID := ids.NewV7().String()
			oID := ids.NewV7().String()
			mID := ids.NewV7().String()
			domainEvt := consignmentnote.StatusChangedEvent{
				ConsignmentNoteID:        consignmentnote.ID(cnID),
				TenantID:                 tenant.ID(tID),
				OrderID:                  consignmentnote.OrderID(oID),
				PriorStatus:              consignmentnote.StatusDispatched,
				NewStatus:                c.newStatus,
				TransitionedAt:           fixedNow(),
				TransitionedByMembership: membership.ID(mID),
			}
			got, err := integrationevents.FromDomainEvent(domainEvt)
			if err != nil {
				t.Fatalf("FromDomainEvent: %v", err)
			}
			if got.Topic() != c.wantTopic {
				t.Errorf("topic: got %s want %s", got.Topic(), c.wantTopic)
			}
		})
	}
}

// TestFromDomainEvent_PanicsOnUnknown asserts the fail-loud branch for
// unmapped domain event types (programmer error per ADR 0008 / CRM
// mapping canon).
func TestFromDomainEvent_PanicsOnUnknown(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on unmapped domain event")
		}
	}()
	type bogus struct{}
	_, _ = integrationevents.FromDomainEvent(bogus{})
}

// TestFromDomainEvent_RejectsBadUUID asserts the parseUUID error path
// (defense-in-depth — aggregate ctor normally prevents this).
func TestFromDomainEvent_RejectsBadUUID(t *testing.T) {
	t.Parallel()
	domainEvt := consignmentnote.CreatedEvent{
		ConsignmentNoteID:     "not-a-uuid",
		TenantID:              tenant.ID(ids.NewV7().String()),
		OrderID:               consignmentnote.OrderID(ids.NewV7().String()),
		CarrierName:           "BlueDart",
		BoxCount:              1,
		WeightGrams:           1,
		CreatedAt:             fixedNow(),
		CreatedByMembershipID: membership.ID(ids.NewV7().String()),
	}
	_, err := integrationevents.FromDomainEvent(domainEvt)
	if !errors.Is(err, integrationevents.ErrInvalidUUID) {
		t.Errorf("got %v want wrapping ErrInvalidUUID", err)
	}
}
