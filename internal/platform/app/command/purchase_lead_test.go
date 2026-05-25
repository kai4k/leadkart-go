package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

// seedAvailableLead inserts a fresh PlatformLead into the leads fake.
func seedAvailableLead(t *testing.T, leads *platformtest.FakePlatformLeadRepository) platformlead.ID {
	t.Helper()
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	contactID := unverifiedcontact.ID(ids.NewV7().String())
	leadID := platformlead.ID(ids.NewV7().String())
	l, err := platformlead.NewFromUnverifiedContact(leadID, contactID, sampleForm(t), agentID, nowFunc())
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	if err := leads.Add(t.Context(), l); err != nil {
		t.Fatalf("seed lead persist: %v", err)
	}
	return leadID
}

// seedCreditedTenant tops up the tenant's lead-credit balance via the
// repository directly (skip the handler — keep this fast).
func seedCreditedTenant(t *testing.T, credits *platformtest.FakeLeadCreditRepository, tenant leadcredit.TenantID, balance int64) {
	t.Helper()
	c, err := leadcredit.NewForTenant(tenant, nowFunc())
	if err != nil {
		t.Fatalf("seed credit row: %v", err)
	}
	op := leadcredit.MembershipID(ids.NewV7().String())
	if balance > 0 {
		if err := c.Topup(balance, "seed", op, nowFunc()); err != nil {
			t.Fatalf("seed topup: %v", err)
		}
	}
	if err := credits.UpsertWithVersion(t.Context(), c); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
}

func TestPurchaseLead_HappyPath(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	tenantID := platformlead.TenantID(ids.NewV7().String())
	memberID := unverifiedcontact.MembershipID(ids.NewV7().String())
	leadID := seedAvailableLead(t, leads)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantID.String()), 10)

	h := command.NewPurchaseLeadHandler(uow, leads, credits, outbox, nowFunc, func() string { return ids.NewV7().String() })
	out, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: memberID,
		AmountPaisa:            50000,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.PurchaseID == "" {
		t.Error("expected non-empty PurchaseID")
	}

	// Lead is now sold.
	l, _ := leads.GetByID(t.Context(), leadID)
	if l.IsAvailable() {
		t.Error("expected lead to be sold")
	}
	if l.SoldToTenantID() != tenantID {
		t.Errorf("SoldToTenantID=%q", l.SoldToTenantID())
	}

	// Credit was debited (1 credit per lead in Slice 1).
	c, _ := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantID.String()))
	if c.Balance() != 9 {
		t.Errorf("balance=%d want 9", c.Balance())
	}

	// Outbox got LeadPurchasedV1 (LeadCreditAdjustedV1 fires from the
	// mechanical mapper via the leadcredit.AdjustedEvent drain;
	// it shows up in the credits repo's DrainedEvents but not in the
	// app-layer fake outbox).
	if len(outbox.Events) != 1 {
		t.Fatalf("expected 1 outbox event, got %d", len(outbox.Events))
	}
	pe, ok := outbox.Events[0].(integrationevents.LeadPurchasedV1)
	if !ok {
		t.Fatalf("expected LeadPurchasedV1, got %T", outbox.Events[0])
	}
	if pe.AmountPaisa != 50000 {
		t.Errorf("AmountPaisa=%d", pe.AmountPaisa)
	}
	if pe.LeadSnapshot.ContactName != "Test Pharma" {
		t.Errorf("snapshot.ContactName=%q", pe.LeadSnapshot.ContactName)
	}
}

func TestPurchaseLead_InsufficientCredits(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	tenantID := platformlead.TenantID(ids.NewV7().String())
	leadID := seedAvailableLead(t, leads)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantID.String()), 0)

	h := command.NewPurchaseLeadHandler(uow, leads, credits, outbox, nowFunc, func() string { return ids.NewV7().String() })
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
		AmountPaisa:            50000,
	})
	if !errors.Is(err, command.ErrInsufficientCredits) {
		t.Fatalf("expected ErrInsufficientCredits, got %v", err)
	}
	// Lead must remain available.
	l, _ := leads.GetByID(t.Context(), leadID)
	if !l.IsAvailable() {
		t.Error("lead must stay available on failed purchase")
	}
}

func TestPurchaseLead_NoCreditRowYet(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	tenantID := platformlead.TenantID(ids.NewV7().String())
	leadID := seedAvailableLead(t, leads)
	// No credit row at all.

	h := command.NewPurchaseLeadHandler(uow, leads, credits, outbox, nowFunc, func() string { return ids.NewV7().String() })
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
		AmountPaisa:            50000,
	})
	if !errors.Is(err, command.ErrInsufficientCredits) {
		t.Fatalf("expected ErrInsufficientCredits, got %v", err)
	}
}

func TestPurchaseLead_AlreadySold(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	// Use the rollback-aware UoW so the loser's debit gets rolled back
	// when the lead UPDATE fires ErrAlreadySold — mirrors production
	// Postgres ROLLBACK behaviour. See FakeUnitOfWork godoc + H10.
	uow := platformtest.NewFakeUnitOfWork(credits, leads)

	tenantA := platformlead.TenantID(ids.NewV7().String())
	tenantB := platformlead.TenantID(ids.NewV7().String())
	leadID := seedAvailableLead(t, leads)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantA.String()), 10)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantB.String()), 10)

	h := command.NewPurchaseLeadHandler(uow, leads, credits, outbox, nowFunc, func() string { return ids.NewV7().String() })

	// First purchase succeeds.
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantA,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
		AmountPaisa:            50000,
	})
	if err != nil {
		t.Fatalf("first purchase: %v", err)
	}

	// Second tenant's purchase fails with ErrLeadAlreadySold.
	_, err = h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantB,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
		AmountPaisa:            50000,
	})
	if !errors.Is(err, command.ErrLeadAlreadySold) {
		t.Fatalf("expected ErrLeadAlreadySold, got %v", err)
	}

	// H10 — loser's balance MUST remain unchanged. The current
	// implementation relies on the surrounding UoW.WithinTx to
	// rollback the credit debit when the platformlead UPDATE fires
	// ErrAlreadySold inside the same closure. The fake UoW runs the
	// closure inline + the fake credit repo persists on
	// UpsertWithVersion — so a regression that moves the
	// platformlead.Purchase mutation BEFORE the credit
	// UpsertWithVersion (or splits them across separate WithinTx
	// closures) would silently debit tenantB. This assertion catches
	// that regression at unit-test time, NOT in production.
	bBal, err := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantB.String()))
	if err != nil {
		t.Fatalf("tenantB credit reload: %v", err)
	}
	if bBal.Balance() != 10 {
		t.Errorf("tenantB balance: got %d want 10 (loser must not be debited on already-sold rejection)", bBal.Balance())
	}
}

func TestPurchaseLead_RetriesOnConflict(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	tenantID := platformlead.TenantID(ids.NewV7().String())
	leadID := seedAvailableLead(t, leads)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantID.String()), 10)

	// First attempt returns ErrConflict; second succeeds.
	credits.ForceConflictOnce = true

	h := command.NewPurchaseLeadHandler(uow, leads, credits, outbox, nowFunc, func() string { return ids.NewV7().String() })
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
		AmountPaisa:            50000,
	})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
}
