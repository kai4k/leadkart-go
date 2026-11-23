package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote/consignmentnotetest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeUoW runs fn synchronously — minimum pg.UnitOfWork impl for
// handler unit tests. Mirrors the shape platformtest.FakeUnitOfWork
// uses but without rollback-aware fakes (single-aggregate command).
type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func sampleCreateCmd() command.CreateConsignmentNoteCommand {
	return command.CreateConsignmentNoteCommand{
		TenantID:              tenant.ID(ids.NewV7().String()),
		OrderID:               consignmentnote.OrderID(ids.NewV7().String()),
		CarrierName:           "BlueDart",
		BoxCount:              3,
		WeightGrams:           7500,
		CreatedByMembershipID: membership.ID(ids.NewV7().String()),
	}
}

func newCreateHandler(t *testing.T) (command.CreateConsignmentNoteHandler, *consignmentnotetest.FakeRepository) {
	t.Helper()
	repo := consignmentnotetest.NewFakeRepository()
	h := command.NewCreateConsignmentNoteHandler(
		fakeUoW{},
		repo,
		fixedNow,
		func() consignmentnote.ID { return consignmentnote.ID(ids.NewV7().String()) },
	)
	return h, repo
}

func TestCreateConsignmentNoteHandler_HappyPath(t *testing.T) {
	t.Parallel()
	h, repo := newCreateHandler(t)
	cmd := sampleCreateCmd()

	out, err := h.Handle(t.Context(), cmd)
	require.NoError(t, err)
	if out.AlreadyExisted {
		t.Error("AlreadyExisted=true on fresh tenant+order")
	}
	if out.ConsignmentNoteID.IsZero() {
		t.Error("ConsignmentNoteID is zero")
	}

	stored, err := repo.GetByOrderID(t.Context(), cmd.TenantID, cmd.OrderID)
	require.NoError(t, err)
	if stored.Status() != consignmentnote.StatusPending {
		t.Errorf("status=%s want pending", stored.Status())
	}
	if stored.CarrierName() != "BlueDart" {
		t.Errorf("carrier=%s", stored.CarrierName())
	}
}

func TestCreateConsignmentNoteHandler_IdempotentOnReplay(t *testing.T) {
	t.Parallel()
	h, repo := newCreateHandler(t)
	cmd := sampleCreateCmd()

	first, err := h.Handle(t.Context(), cmd)
	require.NoError(t, err)

	second, err := h.Handle(t.Context(), cmd)
	require.NoError(t, err)
	if !second.AlreadyExisted {
		t.Error("AlreadyExisted=false on replay")
	}
	if second.ConsignmentNoteID != first.ConsignmentNoteID {
		t.Errorf("replay produced different ID: first=%s second=%s",
			first.ConsignmentNoteID, second.ConsignmentNoteID)
	}
	if got := len(repo.ByOrderID); got != 1 {
		t.Errorf("ByOrderID len=%d want 1", got)
	}
}

func TestCreateConsignmentNoteHandler_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	h, _ := newCreateHandler(t)
	cmd := sampleCreateCmd()
	cmd.TenantID = ""
	_, err := h.Handle(t.Context(), cmd)
	if err == nil {
		t.Fatal("want error on missing tenant_id")
	}
}

func TestCreateConsignmentNoteHandler_RejectsMissingOrder(t *testing.T) {
	t.Parallel()
	h, _ := newCreateHandler(t)
	cmd := sampleCreateCmd()
	cmd.OrderID = ""
	_, err := h.Handle(t.Context(), cmd)
	if err == nil {
		t.Fatal("want error on missing order_id")
	}
}

// TestCreateConsignmentNoteHandler_PropagatesAggregateInvariantErrors
// proves command surfaces aggregate ctor failures (e.g., empty
// carrier_name → consignmentnote.ErrInvalid). The handler returns
// the wrapped error so the HTTP layer can map to 422.
func TestCreateConsignmentNoteHandler_PropagatesAggregateInvariantErrors(t *testing.T) {
	t.Parallel()
	h, _ := newCreateHandler(t)
	cmd := sampleCreateCmd()
	cmd.CarrierName = "  " // whitespace-only → consignmentnote.New rejects with ErrInvalid

	_, err := h.Handle(t.Context(), cmd)
	if !errors.Is(err, consignmentnote.ErrInvalid) {
		t.Errorf("got %v want wrapping consignmentnote.ErrInvalid", err)
	}
}

// I1: a pack accepted at 10:00 with delivery at 11:00, consumed at 12:00 (an
// outage, a backlog, a dead-letter replay), must still get its consignment note:
// the event path judges the delivery date at the moment the order was packed.
// The HTTP create route, which has no pack moment, keeps judging at "now".
func TestCreateConsignmentNoteHandler_DeliveryDateIsJudgedAtThePackMoment(t *testing.T) {
	t.Parallel()
	packedAt := fixedNow().Add(-2 * time.Hour) // 10:00
	eta := fixedNow().Add(-time.Hour)          // 11:00: future at pack, past at the worker's 12:00

	h, repo := newCreateHandler(t)
	cmd := sampleCreateCmd()
	cmd.ExpectedDeliveryAt = &eta
	cmd.RulesJudgedAt = packedAt
	out, err := h.Handle(t.Context(), cmd)
	require.NoError(t, err, "a delivery date that was valid when the order was packed must never be refused later")
	stored, err := repo.GetByOrderID(t.Context(), cmd.TenantID, cmd.OrderID)
	require.NoError(t, err)
	require.Equal(t, out.ConsignmentNoteID, stored.ID())
	require.True(t, stored.CreatedAt().Equal(fixedNow()), "the note is still created NOW, not at the pack moment")

	// A date already past at the pack moment stays refused on the event path.
	h2, _ := newCreateHandler(t)
	stale := packedAt.Add(-time.Minute)
	bad := sampleCreateCmd()
	bad.ExpectedDeliveryAt = &stale
	bad.RulesJudgedAt = packedAt
	_, err = h2.Handle(t.Context(), bad)
	require.ErrorIs(t, err, consignmentnote.ErrInvalid)

	// The HTTP route (no pack moment) judges at now: 11:00 is past at 12:00.
	h3, _ := newCreateHandler(t)
	direct := sampleCreateCmd()
	direct.ExpectedDeliveryAt = &eta
	_, err = h3.Handle(t.Context(), direct)
	require.ErrorIs(t, err, consignmentnote.ErrInvalid)
}
