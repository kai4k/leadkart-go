//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// SQL-CONTRACT COVERAGE (per ADR 0062 — TDL Test Pyramid):
//   - Outbox row insertion in the SAME tx as the aggregate write
//     (ADR 0008), verified end-to-end via the production CRM forwarder
//     + in-process Watermill subscriber. Strict TDL canon per ADR 0062
//     Amendment 1: assertions are subscriber-side, not table-state-side.
//
// Round-trip Add/GetByID/GetBySourcePurchaseID + state-machine
// (ChangeStage / Convert / Assign) + ErrNotFound + ListPage filter
// coverage moved to [crmleadtest.FakeRepository]. ListPage index gate
// stays in keyset_explain_integration_test.go.
//
// PARALLELISM POLICY: outbox-observing tests use newCrmOutboxFixture
// + sharedPG.TruncateAll(t) + NO t.Parallel (strict-TDL outbox rule).

package adapters_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/adapters"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	crmevents "github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

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
// per ADR 0008. Verified end-to-end: production CRM forwarder drains
// the outbox + the in-process Watermill subscriber receives the event.
// Strict TDL canon per ADR 0062 Amendment 1.
//
// arch-test:no-parallel — subscriber-fixture test; uses TruncateAll.
func TestCrmLeadRepository_Add_PersistsAndEmitsOutbox(t *testing.T) {
	sharedPG.TruncateAll(t)
	pool := crmRepoFixture(t)
	repo := adapters.NewCrmLeadRepository(pool, pg.NewTransactor(pool))

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

	// Producer contract (ADR 0064): the Add wrote one enveloped row to the
	// shared common.outbox relay, in the same tx. messagingtest.DrainOutbox
	// reads + unwraps it (the forwarder hop is library code).
	rows := messagingtest.DrainOutbox(t.Context(), t, pool)
	got := messagingtest.EventTypes(rows)
	if len(got) != 1 || got[0] != "crm.lead_created.v1" {
		t.Fatalf("event_types: got %v want [crm.lead_created.v1]", got)
	}
	if rows[0].DestinationTopic != crmevents.Topic {
		t.Fatalf("destination topic: got %q want %q", rows[0].DestinationTopic, crmevents.Topic)
	}
	if tid := rows[0].Message.Metadata.Get(messaging.HeaderTenantID); tid != tenantID.String() {
		t.Fatalf("tenant_id header: got %q want %q", tid, tenantID.String())
	}
}
