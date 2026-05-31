package command_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote/consignmentnotetest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// seedPending writes a pending ConsignmentNote into the fake repo
// + returns (tenantID, cnID, actor) for the test to drive transitions.
func seedPending(t *testing.T, repo *consignmentnotetest.FakeRepository) (tenant.ID, consignmentnote.ID, membership.ID) {
	t.Helper()
	tID := tenant.ID(ids.NewV7().String())
	actor := membership.ID(ids.NewV7().String())
	cn, err := consignmentnote.New(consignmentnote.NewInput{
		ID:                    consignmentnote.ID(ids.NewV7().String()),
		TenantID:              tID,
		OrderID:               consignmentnote.OrderID(ids.NewV7().String()),
		CarrierName:           "BlueDart",
		BoxCount:              3,
		WeightGrams:           7500,
		CreatedByMembershipID: actor,
		Now:                   fixedNow(),
	})
	require.NoError(t, err)
	require.NoError(t, repo.Add(t.Context(), cn))
	return tID, cn.ID(), actor
}

// ----- MarkDispatchedHandler -----

func TestMarkDispatchedHandler_HappyPath(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkDispatchedHandler(repo, fixedNow)

	require.NoError(t, h.Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID:                 tID,
		ConsignmentNoteID:        cnID,
		DocketNumber:             "BDX-555",
		TransitionedByMembership: actor,
	}))

	got, err := repo.GetByID(t.Context(), tID, cnID)
	require.NoError(t, err)
	if got.Status() != consignmentnote.StatusDispatched {
		t.Errorf("status=%s want dispatched", got.Status())
	}
	if got.DocketNumber() != "BDX-555" {
		t.Errorf("docket=%q want BDX-555", got.DocketNumber())
	}
}

func TestMarkDispatchedHandler_RejectsEmptyDocket(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkDispatchedHandler(repo, fixedNow)

	err := h.Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID:                 tID,
		ConsignmentNoteID:        cnID,
		DocketNumber:             "",
		TransitionedByMembership: actor,
	})
	if !errors.Is(err, consignmentnote.ErrInvalid) {
		t.Errorf("got %v want wrapping consignmentnote.ErrInvalid", err)
	}
}

// ----- MarkInTransitHandler -----

func TestMarkInTransitHandler_HappyPath(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	dispatchH := command.NewMarkDispatchedHandler(repo, fixedNow)
	transitH := command.NewMarkInTransitHandler(repo, fixedNow)

	// Walk pending → dispatched → in_transit.
	require.NoError(t, dispatchH.Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID: tID, ConsignmentNoteID: cnID, DocketNumber: "BDX-1", TransitionedByMembership: actor,
	}))
	require.NoError(t, transitH.Handle(t.Context(), command.MarkInTransitCommand{
		TenantID: tID, ConsignmentNoteID: cnID, TransitionedByMembership: actor,
	}))

	got, _ := repo.GetByID(t.Context(), tID, cnID)
	if got.Status() != consignmentnote.StatusInTransit {
		t.Errorf("status=%s want in_transit", got.Status())
	}
}

func TestMarkInTransitHandler_RejectsFromPending(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkInTransitHandler(repo, fixedNow)

	// pending → in_transit is NOT a permitted forward edge.
	err := h.Handle(t.Context(), command.MarkInTransitCommand{
		TenantID: tID, ConsignmentNoteID: cnID, TransitionedByMembership: actor,
	})
	if !errors.Is(err, consignmentnote.ErrInvalidTransition) {
		t.Errorf("got %v want consignmentnote.ErrInvalidTransition", err)
	}
}

// ----- MarkDeliveredHandler -----

func TestMarkDeliveredHandler_FromDispatched(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	dispatchH := command.NewMarkDispatchedHandler(repo, fixedNow)
	deliverH := command.NewMarkDeliveredHandler(repo, fixedNow)

	require.NoError(t, dispatchH.Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID: tID, ConsignmentNoteID: cnID, DocketNumber: "BDX-9", TransitionedByMembership: actor,
	}))
	// Some carriers skip in_transit + go straight to delivered.
	require.NoError(t, deliverH.Handle(t.Context(), command.MarkDeliveredCommand{
		TenantID: tID, ConsignmentNoteID: cnID, TransitionedByMembership: actor,
	}))

	got, _ := repo.GetByID(t.Context(), tID, cnID)
	if got.Status() != consignmentnote.StatusDelivered {
		t.Errorf("status=%s want delivered", got.Status())
	}
}

// ----- MarkFailedHandler -----

func TestMarkFailedHandler_FromAnyNonTerminal(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkFailedHandler(repo, fixedNow)

	require.NoError(t, h.Handle(t.Context(), command.MarkFailedCommand{
		TenantID: tID, ConsignmentNoteID: cnID,
		Reason: "address not found", TransitionedByMembership: actor,
	}))

	got, _ := repo.GetByID(t.Context(), tID, cnID)
	if got.Status() != consignmentnote.StatusFailed {
		t.Errorf("status=%s want failed", got.Status())
	}
	if got.FailureReason() != "address not found" {
		t.Errorf("reason=%q", got.FailureReason())
	}
}

func TestMarkFailedHandler_RejectsEmptyReason(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkFailedHandler(repo, fixedNow)

	err := h.Handle(t.Context(), command.MarkFailedCommand{
		TenantID: tID, ConsignmentNoteID: cnID,
		Reason: "  ", TransitionedByMembership: actor,
	})
	if !errors.Is(err, consignmentnote.ErrInvalid) {
		t.Errorf("got %v want wrapping consignmentnote.ErrInvalid", err)
	}
}
