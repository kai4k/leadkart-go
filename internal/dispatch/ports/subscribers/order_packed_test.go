package subscribers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote/consignmentnotetest"
	"github.com/leadkart/leadkart-go/internal/dispatch/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeUoW is a minimal pg.UnitOfWork that just runs fn synchronously.
// Mirrors the platformtest.FakeUnitOfWork pattern but without
// rollback-aware fakes (single-aggregate test path).
type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func buildHandler(t *testing.T) (*subscribers.OrderPackedIngestor, *consignmentnotetest.FakeRepository) {
	t.Helper()
	repo := consignmentnotetest.NewFakeRepository()
	cmd := command.NewCreateConsignmentNoteHandler(
		fakeUoW{},
		repo,
		fixedNow,
		func() consignmentnote.ID { return consignmentnote.ID(ids.NewV7().String()) },
	)
	return subscribers.NewOrderPackedIngestor(cmd, silentLog()), repo
}

func buildEnvelope(t *testing.T, evt subscribers.OrderPackedV1) *message.Message {
	t.Helper()
	body, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg := message.NewMessage(uuid.NewString(), body)
	msg.Metadata.Set(messaging.HeaderEventType, subscribers.OrderPackedTopic)
	msg.Metadata.Set(messaging.HeaderTenantID, evt.TenantID)
	return msg
}

func validEvent(tenantID, orderID string) subscribers.OrderPackedV1 {
	return subscribers.OrderPackedV1{
		OrderID:              orderID,
		TenantID:             tenantID,
		BoxCount:             3,
		WeightGrams:          7500,
		CarrierName:          "BlueDart",
		PackedAtUTC:          fixedNow().Add(-time.Hour),
		PackedByMembershipID: ids.NewV7().String(),
	}
}

// TestOrderPackedIngestor_HappyPath proves the full chain works:
// envelope decoded → command run → ConsignmentNote created with the
// right tenant + order + carrier.
func TestOrderPackedIngestor_HappyPath(t *testing.T) {
	t.Parallel()
	h, repo := buildHandler(t)
	tenantID := ids.NewV7().String()
	orderID := ids.NewV7().String()

	if err := h.Handle(t.Context(), "", buildEnvelope(t, validEvent(tenantID, orderID))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	cn, err := repo.GetByOrderID(t.Context(), tenant.ID(tenantID), consignmentnote.OrderID(orderID))
	if err != nil {
		t.Fatalf("GetByOrderID: %v", err)
	}
	if cn.Status() != consignmentnote.StatusPending {
		t.Errorf("status=%s want pending", cn.Status())
	}
	if cn.CarrierName() != "BlueDart" {
		t.Errorf("carrier=%s", cn.CarrierName())
	}
	if cn.BoxCount() != 3 {
		t.Errorf("box count=%d", cn.BoxCount())
	}

	// Domain event must have been DRAINED into the fake repo's
	// DrainedEvents slice — proves the events-are-raised-properly
	// invariant the user asked about.
	if len(repo.DrainedEvents) != 1 {
		t.Fatalf("DrainedEvents len=%d want 1", len(repo.DrainedEvents))
	}
	if _, ok := repo.DrainedEvents[0].(consignmentnote.CreatedEvent); !ok {
		t.Errorf("drained event type=%T want CreatedEvent", repo.DrainedEvents[0])
	}
}

// TestOrderPackedIngestor_IdempotentOnReplay proves duplicate delivery
// produces ONE ConsignmentNote (the natural-key precheck catches the
// second).
func TestOrderPackedIngestor_IdempotentOnReplay(t *testing.T) {
	t.Parallel()
	h, repo := buildHandler(t)
	tenantID := ids.NewV7().String()
	orderID := ids.NewV7().String()
	env := buildEnvelope(t, validEvent(tenantID, orderID))

	if err := h.Handle(t.Context(), "", env); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := h.Handle(t.Context(), "", env); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := len(repo.ByOrderID); got != 1 {
		t.Errorf("ByOrderID len=%d want 1 (idempotent)", got)
	}
}

// TestOrderPackedIngestor_WrongTopicShortCircuits proves the
// event_type filter — the subscriber ignores events from the same
// topic that aren't OrderPacked.
func TestOrderPackedIngestor_WrongTopicShortCircuits(t *testing.T) {
	t.Parallel()
	h, repo := buildHandler(t)
	msg := buildEnvelope(t, validEvent(ids.NewV7().String(), ids.NewV7().String()))
	msg.Metadata.Set(messaging.HeaderEventType, "orders.order_confirmed.v1")
	if err := h.Handle(t.Context(), "", msg); err != nil {
		t.Fatalf("Handle wrong topic: %v", err)
	}
	if len(repo.Store) != 0 {
		t.Error("short-circuit failed: a consignment note was created for the wrong event_type")
	}
}

// TestOrderPackedIngestor_MalformedPayloadErrors proves bad JSON
// produces an error (which causes Watermill retry).
func TestOrderPackedIngestor_MalformedPayloadErrors(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t)
	msg := message.NewMessage(uuid.NewString(), []byte("{not json"))
	msg.Metadata.Set(messaging.HeaderEventType, subscribers.OrderPackedTopic)
	if err := h.Handle(t.Context(), "", msg); err == nil {
		t.Fatal("want decode error")
	}
}

// TestOrderPackedIngestor_RejectsMissingIDs proves defensive rejection
// when the envelope is structurally valid but missing required IDs.
func TestOrderPackedIngestor_RejectsMissingIDs(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t)

	evt := validEvent(ids.NewV7().String(), ids.NewV7().String())
	evt.OrderID = ""
	msg := buildEnvelope(t, evt)
	if err := h.Handle(t.Context(), "", msg); err == nil {
		t.Fatal("want error on missing order_id")
	}
}

// TestOrderPackedIngestor_DefaultsCarrierWhenAbsent proves "Unassigned"
// fallback for OrderPacked envelopes that don't pre-pick a carrier.
func TestOrderPackedIngestor_DefaultsCarrierWhenAbsent(t *testing.T) {
	t.Parallel()
	h, repo := buildHandler(t)
	tenantID := ids.NewV7().String()
	orderID := ids.NewV7().String()
	evt := validEvent(tenantID, orderID)
	evt.CarrierName = "  "

	if err := h.Handle(t.Context(), "", buildEnvelope(t, evt)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	cn, err := repo.GetByOrderID(t.Context(), tenant.ID(tenantID), consignmentnote.OrderID(orderID))
	if err != nil {
		t.Fatalf("GetByOrderID: %v", err)
	}
	if cn.CarrierName() != "Unassigned" {
		t.Errorf("carrier=%s want Unassigned", cn.CarrierName())
	}
}
