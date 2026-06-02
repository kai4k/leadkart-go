package command_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

func TestInitialiseLeadCredits_CreatesZeroBalanceRow(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.NewFakeUnitOfWork(credits)

	tenantID := leadcredit.TenantID(ids.NewV7().String())

	h := command.NewInitialiseLeadCreditsHandler(uow, credits, nowFunc)
	out, err := h.Handle(t.Context(), command.InitialiseLeadCreditsCommand{TenantID: tenantID})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.AlreadyExisted {
		t.Fatal("AlreadyExisted=true on fresh tenant")
	}

	stored, err := credits.GetByTenant(t.Context(), tenantID)
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if stored.Balance() != 0 {
		t.Errorf("Balance=%d want 0", stored.Balance())
	}
}

func TestInitialiseLeadCredits_IdempotentOnReplay(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.NewFakeUnitOfWork(credits)

	tenantID := leadcredit.TenantID(ids.NewV7().String())

	h := command.NewInitialiseLeadCreditsHandler(uow, credits, nowFunc)
	if _, err := h.Handle(t.Context(), command.InitialiseLeadCreditsCommand{TenantID: tenantID}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	out, err := h.Handle(t.Context(), command.InitialiseLeadCreditsCommand{TenantID: tenantID})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !out.AlreadyExisted {
		t.Error("AlreadyExisted=false on replay")
	}
}

func TestInitialiseLeadCredits_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.NewFakeUnitOfWork(credits)

	h := command.NewInitialiseLeadCreditsHandler(uow, credits, nowFunc)
	_, err := h.Handle(t.Context(), command.InitialiseLeadCreditsCommand{TenantID: ""})
	if err == nil {
		t.Fatal("expected error on empty tenant_id")
	}
}

// TestInitialiseLeadCredits_PreservesExistingBalanceOnReplay asserts the
// idempotency safety property: a replayed Initialise after a topup has
// happened MUST NOT zero the balance. This is the "permanent purchases"
// invariant from BRD §4.2 expressed at the operation boundary —
// re-delivering TenantRegisteredV1 from the broker (a normal at-least-
// once event) must not destroy credits the tenant has paid for.
func TestInitialiseLeadCredits_PreservesExistingBalanceOnReplay(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.NewFakeUnitOfWork(credits)

	tenantID := leadcredit.TenantID(ids.NewV7().String())
	op := leadcredit.MembershipID(ids.NewV7().String())

	init := command.NewInitialiseLeadCreditsHandler(uow, credits, nowFunc)
	topup := command.NewTopupLeadCreditsHandler(uow, credits, nowFunc)

	if _, err := init.Handle(t.Context(), command.InitialiseLeadCreditsCommand{TenantID: tenantID}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := topup.Handle(t.Context(), command.TopupLeadCreditsCommand{
		TenantID: tenantID, Delta: 250, Reason: "Q3", AdjustedBy: op,
	}); err != nil {
		t.Fatalf("topup: %v", err)
	}

	out, err := init.Handle(t.Context(), command.InitialiseLeadCreditsCommand{TenantID: tenantID})
	if err != nil {
		t.Fatalf("replay init: %v", err)
	}
	if !out.AlreadyExisted {
		t.Error("AlreadyExisted=false on replay after topup")
	}

	stored, err := credits.GetByTenant(t.Context(), tenantID)
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if stored.Balance() != 250 {
		t.Errorf("Balance=%d want 250 (replay must preserve topup)", stored.Balance())
	}
}

// TestInitialiseLeadCredits_TreatsRaceConflictAsAlreadyExisted asserts
// the "concurrent insert" path: when a competing handler (Topup that
// also lazy-creates the row) wins the race, our INSERT returns
// ErrConflict and we must ACK as "already existed" rather than retry
// to infinity.
func TestInitialiseLeadCredits_TreatsRaceConflictAsAlreadyExisted(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.NewFakeUnitOfWork(credits)

	tenantID := leadcredit.TenantID(ids.NewV7().String())
	credits.ForceConflictOnce = true

	h := command.NewInitialiseLeadCreditsHandler(uow, credits, nowFunc)
	out, err := h.Handle(t.Context(), command.InitialiseLeadCreditsCommand{TenantID: tenantID})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !out.AlreadyExisted {
		t.Error("AlreadyExisted=false on conflict; expected true")
	}
}

