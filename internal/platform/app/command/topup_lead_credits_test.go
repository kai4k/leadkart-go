package command_test

import (
	"context"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

func TestTopupLeadCredits_CreatesRowOnFirstTopup(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.FakeUnitOfWork{}

	tenantID := leadcredit.TenantID(ids.NewV7().String())
	op := leadcredit.MembershipID(ids.NewV7().String())

	h := command.NewTopupLeadCreditsHandler(uow, credits, nowFunc)
	out, err := h.Handle(context.Background(), command.TopupLeadCreditsCommand{
		TenantID:   tenantID,
		Delta:      100,
		Reason:     "Q3 marketing budget",
		AdjustedBy: op,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.NewBalance != 100 {
		t.Errorf("NewBalance=%d want 100", out.NewBalance)
	}

	// Subsequent topup increments.
	out, err = h.Handle(context.Background(), command.TopupLeadCreditsCommand{
		TenantID:   tenantID,
		Delta:      50,
		Reason:     "extra",
		AdjustedBy: op,
	})
	if err != nil {
		t.Fatalf("second topup: %v", err)
	}
	if out.NewBalance != 150 {
		t.Errorf("NewBalance=%d want 150", out.NewBalance)
	}
}

func TestTopupLeadCredits_RejectsNonPositiveDelta(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.FakeUnitOfWork{}

	h := command.NewTopupLeadCreditsHandler(uow, credits, nowFunc)
	_, err := h.Handle(context.Background(), command.TopupLeadCreditsCommand{
		TenantID:   leadcredit.TenantID(ids.NewV7().String()),
		Delta:      0,
		Reason:     "x",
		AdjustedBy: leadcredit.MembershipID(ids.NewV7().String()),
	})
	if err == nil {
		t.Fatal("expected error on zero delta")
	}
}

func TestTopupLeadCredits_RetriesOnConflict(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.FakeUnitOfWork{}

	credits.ForceConflictOnce = true

	h := command.NewTopupLeadCreditsHandler(uow, credits, nowFunc)
	out, err := h.Handle(context.Background(), command.TopupLeadCreditsCommand{
		TenantID:   leadcredit.TenantID(ids.NewV7().String()),
		Delta:      100,
		Reason:     "retry test",
		AdjustedBy: leadcredit.MembershipID(ids.NewV7().String()),
	})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if out.NewBalance != 100 {
		t.Errorf("NewBalance=%d", out.NewBalance)
	}
}
