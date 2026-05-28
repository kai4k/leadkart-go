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
