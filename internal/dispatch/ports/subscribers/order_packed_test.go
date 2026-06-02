package subscribers_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote/consignmentnotetest"
	"github.com/leadkart/leadkart-go/internal/dispatch/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// fakeUoW is a minimal pg.UnitOfWork that just runs fn synchronously.
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

func validEvent(tenantID, orderID string) ordersevents.OrderPackedV1 {
	return ordersevents.OrderPackedV1{
		OrderID:              orderID,
		TenantID:             tenantID,
		BoxCount:             3,
		WeightGrams:          7500,
		CarrierName:          "BlueDart",
		PackedAtUTC:          fixedNow().Add(-time.Hour),
		PackedByMembershipID: ids.NewV7().String(),
	}
}

// Post-cqrs (ADR 0067): the handler receives the already-decoded typed
// event; topic routing + payload decode are the EventProcessor's job, so
// the old wrong-event-type + malformed-JSON unit cases are gone (the
// typed handler can never be invoked with a mismatched type or bad bytes).

// TestOrderPackedIngestor_HappyPath proves command run → ConsignmentNote
// created with the right tenant + order + carrier + drained event.
func TestOrderPackedIngestor_HappyPath(t *testing.T) {
	t.Parallel()
	h, repo := buildHandler(t)
	tenantID := ids.NewV7().String()
	orderID := ids.NewV7().String()

	evt := validEvent(tenantID, orderID)
	if err := h.Handle(t.Context(), &evt); err != nil {
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
	evt := validEvent(tenantID, orderID)

	if err := h.Handle(t.Context(), &evt); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := h.Handle(t.Context(), &evt); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := len(repo.ByOrderID); got != 1 {
		t.Errorf("ByOrderID len=%d want 1 (idempotent)", got)
	}
}

// TestOrderPackedIngestor_RejectsMissingIDs proves defensive rejection
// when the typed event is missing required IDs.
func TestOrderPackedIngestor_RejectsMissingIDs(t *testing.T) {
	t.Parallel()
	h, _ := buildHandler(t)
	evt := validEvent(ids.NewV7().String(), ids.NewV7().String())
	evt.OrderID = ""
	if err := h.Handle(t.Context(), &evt); err == nil {
		t.Fatal("want error on missing order_id")
	}
}

// TestOrderPackedIngestor_DefaultsCarrierWhenAbsent proves "Unassigned"
// fallback for OrderPacked events that don't pre-pick a carrier.
func TestOrderPackedIngestor_DefaultsCarrierWhenAbsent(t *testing.T) {
	t.Parallel()
	h, repo := buildHandler(t)
	tenantID := ids.NewV7().String()
	orderID := ids.NewV7().String()
	evt := validEvent(tenantID, orderID)
	evt.CarrierName = "  "

	if err := h.Handle(t.Context(), &evt); err != nil {
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
