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

// seedAvailableLead inserts a fresh standard-tier PlatformLead into the fake.
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

// seedLeadWithSaleLimit inserts a lead carrying a per-lead sale_limit override.
func seedLeadWithSaleLimit(t *testing.T, leads *platformtest.FakePlatformLeadRepository, limit int) platformlead.ID {
	t.Helper()
	leadID := platformlead.ID(ids.NewV7().String())
	l := platformlead.UnmarshalFromDB(platformlead.Snapshot{
		ID:                     leadID,
		SourceContactID:        unverifiedcontact.ID(ids.NewV7().String()),
		Form:                   sampleForm(t),
		Tier:                   platformlead.TierStandard,
		SaleLimit:              &limit,
		VerifiedAt:             nowFunc(),
		VerifiedByMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
		CreatedAt:              nowFunc(),
	})
	if err := leads.Add(t.Context(), l); err != nil {
		t.Fatalf("seed lead persist: %v", err)
	}
	return leadID
}

// seedCreditedTenant tops up the tenant's balance via the repo directly.
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

	h := command.NewPurchaseLeadHandler(uow, leads, credits, platformtest.NewFakeTierReader(), outbox, nowFunc, func() string { return ids.NewV7().String() })
	out, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: memberID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.PurchaseID == "" {
		t.Error("expected non-empty PurchaseID")
	}
	// First buyer pays the tier base price (no volume discount).
	if out.AmountPaisa != 50000 {
		t.Errorf("out.AmountPaisa=%d want 50000", out.AmountPaisa)
	}

	// Lead now has one buyer.
	l, _ := leads.GetByID(t.Context(), leadID)
	if !l.HasBuyer(tenantID) {
		t.Error("expected tenant recorded as buyer")
	}

	// Credit was debited (1 credit per lead).
	c, _ := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantID.String()))
	if c.Balance() != 9 {
		t.Errorf("balance=%d want 9", c.Balance())
	}

	// Only LeadPurchasedV1 hits the app-layer outbox.
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

func TestPurchaseLead_MultipleBuyersUnderLimit(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	tenantA := platformlead.TenantID(ids.NewV7().String())
	tenantB := platformlead.TenantID(ids.NewV7().String())
	leadID := seedAvailableLead(t, leads) // standard limit 6
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantA.String()), 10)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantB.String()), 10)

	h := command.NewPurchaseLeadHandler(uow, leads, credits, platformtest.NewFakeTierReader(), outbox, nowFunc, func() string { return ids.NewV7().String() })

	a, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID: leadID, PurchasingTenantID: tenantA,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if err != nil {
		t.Fatalf("buyer A: %v", err)
	}
	b, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID: leadID, PurchasingTenantID: tenantB,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if err != nil {
		t.Fatalf("buyer B: %v", err)
	}
	// Volume discount: second buyer pays less than the first.
	if !(b.AmountPaisa < a.AmountPaisa) {
		t.Errorf("expected volume discount: B=%d should be < A=%d", b.AmountPaisa, a.AmountPaisa)
	}
	l, _ := leads.GetByID(t.Context(), leadID)
	if !l.HasBuyer(tenantA) || !l.HasBuyer(tenantB) {
		t.Error("expected both tenants recorded as buyers")
	}
}

func TestPurchaseLead_DoubleBuySameTenant_Rejected(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork(credits, leads)

	tenantID := platformlead.TenantID(ids.NewV7().String())
	memberID := unverifiedcontact.MembershipID(ids.NewV7().String())
	leadID := seedAvailableLead(t, leads)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantID.String()), 10)

	h := command.NewPurchaseLeadHandler(uow, leads, credits, platformtest.NewFakeTierReader(), outbox, nowFunc, func() string { return ids.NewV7().String() })

	if _, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID: leadID, PurchasingTenantID: tenantID, PurchasingMembershipID: memberID,
	}); err != nil {
		t.Fatalf("first purchase: %v", err)
	}
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID: leadID, PurchasingTenantID: tenantID, PurchasingMembershipID: memberID,
	})
	if !errors.Is(err, command.ErrLeadAlreadyPurchased) {
		t.Fatalf("expected ErrLeadAlreadyPurchased, got %v", err)
	}
	// The flat credit cost is one (only the first purchase debits).
	c, _ := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantID.String()))
	if c.Balance() != 9 {
		t.Errorf("balance=%d want 9 (re-buy must roll back its debit)", c.Balance())
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

	h := command.NewPurchaseLeadHandler(uow, leads, credits, platformtest.NewFakeTierReader(), outbox, nowFunc, func() string { return ids.NewV7().String() })
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrInsufficientCredits) {
		t.Fatalf("expected ErrInsufficientCredits, got %v", err)
	}
	// Lead must remain available (no buyer recorded).
	l, _ := leads.GetByID(t.Context(), leadID)
	if l.PurchaseCount() != 0 {
		t.Error("lead must stay unbought on failed purchase")
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

	h := command.NewPurchaseLeadHandler(uow, leads, credits, platformtest.NewFakeTierReader(), outbox, nowFunc, func() string { return ids.NewV7().String() })
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrInsufficientCredits) {
		t.Fatalf("expected ErrInsufficientCredits, got %v", err)
	}
}

func TestPurchaseLead_SoldOut_LoserNotDebited(t *testing.T) {
	t.Parallel()

	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	// Rollback-aware UoW: the loser's debit rolls back when RecordPurchase
	// fires ErrSoldOut, mirroring Postgres. See H10.
	uow := platformtest.NewFakeUnitOfWork(credits, leads)

	tenantA := platformlead.TenantID(ids.NewV7().String())
	tenantB := platformlead.TenantID(ids.NewV7().String())
	leadID := seedLeadWithSaleLimit(t, leads, 1) // sells out after one buyer
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantA.String()), 10)
	seedCreditedTenant(t, credits, leadcredit.TenantID(tenantB.String()), 10)

	h := command.NewPurchaseLeadHandler(uow, leads, credits, platformtest.NewFakeTierReader(), outbox, nowFunc, func() string { return ids.NewV7().String() })

	// First buyer takes the only slot.
	if _, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID: leadID, PurchasingTenantID: tenantA,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
	}); err != nil {
		t.Fatalf("first purchase: %v", err)
	}

	// Second buyer is sold out.
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID: leadID, PurchasingTenantID: tenantB,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrLeadSoldOut) {
		t.Fatalf("expected ErrLeadSoldOut, got %v", err)
	}

	// H10 — the loser's balance must be unchanged (debit rolled back when
	// RecordPurchase fired ErrSoldOut in the same closure).
	bBal, err := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantB.String()))
	if err != nil {
		t.Fatalf("tenantB credit reload: %v", err)
	}
	if bBal.Balance() != 10 {
		t.Errorf("tenantB balance: got %d want 10 (loser must not be debited on sold-out rejection)", bBal.Balance())
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

	// First attempt conflicts; the retry succeeds.
	credits.ForceConflictOnce = true

	h := command.NewPurchaseLeadHandler(uow, leads, credits, platformtest.NewFakeTierReader(), outbox, nowFunc, func() string { return ids.NewV7().String() })
	_, err := h.Handle(t.Context(), command.PurchaseLeadCommand{
		PlatformLeadID:         leadID,
		PurchasingTenantID:     tenantID,
		PurchasingMembershipID: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
}
