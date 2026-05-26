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
//
// SQL-CONTRACT COVERAGE (per ADR 0062 — TDL Test Pyramid):
//   - Outbox row insertion in the SAME tx as the aggregate write
//     (ADR 0008); confirms topic + tenant_id stamping on the CRM lead-
//     created event.
//
// Round-trip Add/GetByID/GetBySourcePurchaseID + state-machine
// (ChangeStage / Convert / Assign) + ErrNotFound + ListPage filter
// coverage moved to [crmleadtest.FakeRepository]. ListPage index gate
// stays in keyset_explain_integration_test.go.

package adapters_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/adapters"
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

// TestCrmLeadRepository_Add_PersistsAndEmitsOutbox — SQL-contract:
// every crmlead.Add MUST also write a row to crm.outbox in the same tx
// per ADR 0008. The read-back uses the messagingtest helper (RLS+FORCE
// on outbox bypassed via platform GUC); confirms the canonical topic
// + tenant_id round-trip.
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

	// Outbox row written with topic crm.lead_created.v1. The outbox is
	// RLS+FORCE — assertion goes through the typed messagingtest helper
	// (Wave 7 + ADR 0047: no raw SQL in tests; helper internalises the
	// platform-GUC bypass once).
	topics := messagingtest.OutboxTopicsForTenant(t, pool, messagingtest.SchemaCRM, tenantID.String())
	if len(topics) != 1 || topics[0] != "crm.lead_created.v1" {
		t.Fatalf("outbox topics: %v", topics)
	}
}
