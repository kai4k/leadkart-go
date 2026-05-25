//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via withTenantCtx; RLS isolates
//   rows by tenant so parallel runs cannot see each others state.
//   Brandur "Postgres at scale" + TDL Wild Workouts canon: shared
//   infrastructure + per-test logical isolation = safe parallelism.

package adapters_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/adapters"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Shared bootstrap (crmRepoFixture / TestMain / migrations / role
// provisioning / withTenantCtx) lives in fixture_integration_test.go
// per the Brandur / TDL canon — ONE container per package, shared
// pool, per-test isolation via fresh tenant_id + RLS.

// newSnapshot returns a valid PurchaseSnapshot keyed by a fresh
// PurchaseID — tests use this to seed leads through the subscriber-
// shaped factory path.
func newSnapshot(t *testing.T, purchaseID, platformLeadID, buyer string) crmlead.PurchaseSnapshot {
	t.Helper()
	return crmlead.PurchaseSnapshot{
		PurchaseID:              purchaseID,
		PlatformLeadID:          platformLeadID,
		PurchasedByMembershipID: buyer,
		ContactName:             "Test Pharma Store",
		MobileE164:              "+919812345678",
		Email:                   "owner@example.com",
		PinCode:                 "411001",
		City:                    "Pune",
		District:                "Pune",
		State:                   "Maharashtra",
		Street:                  "MG Road 12",
		HasDrugLicence:          true,
		HasGst:                  true,
		GstNumber:               "27ABCDE1234F1Z5",
		HasPan:                  true,
		PanNumber:               "ABCDE1234F",
		BusinessType:            "PCD",
		MedicineSystem:          "Allopathic",
		ProductRanges:           []string{"Antibiotics", "Cardiac"},
		DosageForms:             []string{"Tablet"},
		OrderValue:              "Upto25000",
		BuyTimeline:             "WithinWeek",
	}
}

func TestCrmLeadRepository_Add_PersistsAndEmitsOutbox(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)

	leadID := crmlead.ID(ids.NewV7().String())
	purchaseID := ids.NewV7().String()
	snap := newSnapshot(t, purchaseID, ids.NewV7().String(), ids.NewV7().String())
	l, err := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()), snap, time.Now().UTC())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	if err := repo.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := repo.GetByID(ctx, leadID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Stage() != crmlead.StageNew {
		t.Fatalf("stage: %v", got.Stage())
	}
	if got.SourcePurchaseID() != purchaseID {
		t.Fatalf("purchase id round-trip: got %q want %q", got.SourcePurchaseID(), purchaseID)
	}
	if got.Profile().Extra.GstNumber != "27ABCDE1234F1Z5" {
		t.Fatalf("extra round-trip: %q", got.Profile().Extra.GstNumber)
	}

	// Outbox row written with topic crm.lead_created.v1. The outbox is
	// RLS+FORCE â€” assertion goes through the typed messagingtest helper
	// (Wave 7 + ADR 0047: no raw SQL in tests; helper internalises the
	// platform-GUC bypass once).
	topics := messagingtest.OutboxTopicsForTenant(t, pool, messagingtest.SchemaCRM, tenantID.String())
	if len(topics) != 1 || topics[0] != "crm.lead_created.v1" {
		t.Fatalf("outbox topics: %v", topics)
	}
}

func TestCrmLeadRepository_GetBySourcePurchaseID_FoundAndNotFound(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)
	leadID := crmlead.ID(ids.NewV7().String())
	purchase := ids.NewV7().String()
	snap := newSnapshot(t, purchase, ids.NewV7().String(), ids.NewV7().String())
	l, err := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()), snap, time.Now().UTC())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if err := repo.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Hit
	got, err := repo.GetBySourcePurchaseID(ctx, purchase)
	if err != nil {
		t.Fatalf("GetBySourcePurchaseID: %v", err)
	}
	if got.ID() != leadID {
		t.Fatalf("id mismatch: got %s want %s", got.ID(), leadID)
	}
	// Miss
	_, err = repo.GetBySourcePurchaseID(ctx, ids.NewV7().String())
	if !errors.Is(err, crmlead.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCrmLeadRepository_UpdateByID_StateMachineRoundTrip(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)
	leadID := crmlead.ID(ids.NewV7().String())
	snap := newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String())
	l, _ := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()), snap, time.Now().UTC())
	if err := repo.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}

	actor := ids.NewV7().String()
	if err := repo.UpdateByID(ctx, leadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.ChangeStage(crmlead.StageContacted, actor, "first call", time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateByID stage: %v", err)
	}
	got, err := repo.GetByID(ctx, leadID)
	if err != nil {
		t.Fatalf("GetByID after stage: %v", err)
	}
	if got.Stage() != crmlead.StageContacted {
		t.Fatalf("stage round-trip: %v", got.Stage())
	}

	if err := repo.UpdateByID(ctx, leadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.Convert(actor, time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateByID convert: %v", err)
	}
	got2, _ := repo.GetByID(ctx, leadID)
	if got2.Stage() != crmlead.StageConverted {
		t.Fatalf("convert: %v", got2.Stage())
	}
	if got2.ConvertedByMembershipID() != actor {
		t.Fatalf("convert actor: %q", got2.ConvertedByMembershipID())
	}
}

func TestCrmLeadRepository_ListPage_FilterByStage(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)

	// Seed two leads, advance one to Contacted.
	first := crmlead.ID(ids.NewV7().String())
	l1, _ := crmlead.NewFromPurchaseSnapshot(first, tenant.ID(tenantID.String()),
		newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String()), time.Now().UTC())
	if err := repo.Add(ctx, l1); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	second := crmlead.ID(ids.NewV7().String())
	l2, _ := crmlead.NewFromPurchaseSnapshot(second, tenant.ID(tenantID.String()),
		newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String()), time.Now().UTC())
	if err := repo.Add(ctx, l2); err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	actor := ids.NewV7().String()
	if err := repo.UpdateByID(ctx, second, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.ChangeStage(crmlead.StageContacted, actor, "", time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	// Filter by stage=contacted should return ONLY the second lead.
	page, err := repo.ListPage(ctx, crmlead.ListFilter{Stage: crmlead.StageContacted}, pagination.Cursor{}, 50)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID() != second {
		t.Fatalf("filter stage=contacted returned wrong set: %+v", page.Items)
	}

	// No filter â†’ both leads.
	full, err := repo.ListPage(ctx, crmlead.ListFilter{}, pagination.Cursor{}, 50)
	if err != nil {
		t.Fatalf("ListPage unfiltered: %v", err)
	}
	if len(full.Items) != 2 {
		t.Fatalf("unfiltered count: %d", len(full.Items))
	}
}

func TestCrmLeadRepository_Assign_AndCallLog_RoundTrip(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	leads := adapters.NewCrmLeadRepository(pool, tx)
	calls := adapters.NewCallLogRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)
	leadID := crmlead.ID(ids.NewV7().String())
	l, _ := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()),
		newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String()), time.Now().UTC())
	if err := leads.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Assign via UpdateByID â€” handler-side path uses a UoW to combine
	// the lead update + history insert; this test exercises only the
	// repo-side persistence.
	assignee := ids.NewV7().String()
	manager := ids.NewV7().String()
	if err := leads.UpdateByID(ctx, leadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.Assign(assignee, manager, "first routing", time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	got, _ := leads.GetByID(ctx, leadID)
	if got.AssigneeMembershipID() != assignee {
		t.Fatalf("assignee: %q", got.AssigneeMembershipID())
	}

	// Append a call log.
	callID := calllog.ID(ids.NewV7().String())
	cl, err := calllog.New(callID, tenant.ID(tenantID.String()), leadID, calllog.OutcomeInterested, "warm", assignee, time.Now().UTC())
	if err != nil {
		t.Fatalf("calllog.New: %v", err)
	}
	if err := calls.Add(ctx, cl); err != nil {
		t.Fatalf("calls.Add: %v", err)
	}
	rows, err := calls.ListByLead(ctx, leadID)
	if err != nil {
		t.Fatalf("ListByLead: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome() != calllog.OutcomeInterested {
		t.Fatalf("calls row: %+v", rows)
	}
}
