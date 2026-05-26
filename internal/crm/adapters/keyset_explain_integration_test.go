//go:build integration

// arch-test:no-timeout-needed — integration test relies on testcontainers boot timeout.
// arch-test:no-synctest — testcontainers Postgres can't be virtualised by synctest.

// keyset_explain_integration_test.go — ADR 0038 EXPLAIN gate.
//
// Asserts the cursor-paginated keyset query on crm.crm_leads
// plans as an Index Scan against idx_crm_leads_tenant_stage_created,
// not a Seq Scan + Filter. Even under RLS predicates.
//
// Mirrors the identity-side canonical example at
// internal/identity/adapters/keyset_explain_integration_test.go.

package adapters_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pg/rlstest"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/crm/adapters"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// TestKeysetCrmLeadsPage_UsesIndexUnderRLS verifies the canon
// discipline from ADR 0038: the cursor-paginated keyset query
// MUST plan as an Index Scan against idx_crm_leads_tenant_stage_created,
// not a Seq Scan + Filter. Even under RLS predicates.
//
// Test shape:
//
//  1. Bring up testcontainers Postgres + apply migrations.
//  2. Seed 200 active CRM leads in one tenant (enough rows that
//     the planner prefers index over seq scan).
//  3. Set RLS GUC to the test tenant.
//  4. Run EXPLAIN (FORMAT JSON) on the same keyset query the
//     ListCrmLeadsPage sqlc query emits.
//  5. Assert the plan contains "Index Scan" referencing the
//     expected composite index.
//
// If this test fails, the planner has either gained a regression
// OR a future migration broke the (tenant_id, ..., created_at, id DESC)
// composite — both warrant a manual EXPLAIN review.
func TestKeysetCrmLeadsPage_UsesIndexUnderRLS(t *testing.T) {
	pool := crmRepoFixture(t)
	ctx := t.Context()

	// --- Seed -------------------------------------------------------------
	tx := pg.NewTransactor(pool)
	leads := adapters.NewCrmLeadRepository(pool, tx)

	tenantUUID := ids.NewV7()
	tenantID := tenant.ID(tenantUUID.String())
	tctx := tenancy.WithID(ctx, tenancy.ID(tenantUUID.String()))

	const seedCount = 200
	for i := 0; i < seedCount; i++ {
		purchaseID := ids.NewV7().String()
		snap := newSnapshot(t,
			purchaseID,
			ids.NewV7().String(),
			ids.NewV7().String(),
		)
		// Stagger CreatedAt by 1ms so the keyset cursor has a deterministic order.
		now := time.Date(2026, 6, 2, 9, 0, 0, i*1_000_000, time.UTC)
		l, err := crmlead.NewFromPurchaseSnapshot(crmlead.ID(ids.NewV7().String()), tenantID, snap, now)
		if err != nil {
			t.Fatalf("factory %d: %v", i, err)
		}
		if err := leads.Add(tctx, l); err != nil {
			t.Fatalf("seed lead %d: %v", i, err)
		}
	}

	// ANALYZE so the planner has up-to-date statistics. Without it
	// fresh-seeded data may not yet be reflected in pg_stat.
	if _, err := pool.Exec(ctx, `ANALYZE crm.crm_leads`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// --- EXPLAIN under RLS scope ----------------------------------------
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// SET LOCAL only takes effect inside a tx.
	dbtx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = dbtx.Rollback(ctx) }()

	// Bind the test tenant's GUC so RLS admits rows.
	rlstest.SetSessionTenant(t, ctx, dbtx, tenantUUID.String())

	// The same predicate shape sqlc emits in ListCrmLeadsPage —
	// stage filter narrows to a partial-index path against
	// idx_crm_leads_tenant_stage_created. First-page sentinel cursor
	// admits every row.
	const explainSQL = `
		EXPLAIN (FORMAT JSON, ANALYZE, BUFFERS)
		SELECT id, tenant_id, stage, temperature, created_at
		FROM   crm.crm_leads
		WHERE  tenant_id = $1
		  AND  stage = $2
		  AND  (created_at, id) < ($3::timestamptz, $4::uuid)
		ORDER  BY created_at DESC, id DESC
		LIMIT  51
	`
	sentinelTime := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	sentinelID := "ffffffff-ffff-ffff-ffff-ffffffffffff"

	var rawPlan []byte
	if err := dbtx.QueryRow(ctx, explainSQL, tenantUUID, "new", sentinelTime, sentinelID).Scan(&rawPlan); err != nil {
		t.Fatalf("explain: %v", err)
	}

	planText := string(rawPlan)
	t.Logf("EXPLAIN plan:\n%s", planText)

	// Permissive assertion: the plan must contain "Index Scan" AND
	// reference the expected index. We don't pin the exact plan-tree
	// shape because the planner may add Merge/Sort/Limit nodes around
	// the Index Scan — what matters is the index is in use.
	if !strings.Contains(planText, "Index Scan") {
		t.Errorf("expected plan to contain \"Index Scan\"; got:\n%s", planText)
	}
	if !strings.Contains(planText, "idx_crm_leads_tenant_stage_created") {
		t.Errorf("expected plan to reference idx_crm_leads_tenant_stage_created; got:\n%s", planText)
	}
	// Bonus: Seq Scan on crm_leads would be a regression.
	if strings.Contains(planText, `"Node Type": "Seq Scan"`) &&
		strings.Contains(planText, `"Relation Name": "crm_leads"`) {
		t.Errorf("planner fell back to Seq Scan on crm_leads:\n%s", planText)
	}

	// Also verify the JSON is well-formed (catches future planner
	// output-format changes early).
	var parsed []map[string]any
	if err := json.Unmarshal(rawPlan, &parsed); err != nil {
		t.Fatalf("plan JSON malformed: %v", err)
	}
}

// _ keeps the context import live even if future refactors collapse
// the test body.
var _ context.Context = context.Background() // arch-test:context-background — package-level type assertion, no *testing.T in scope

// _ keeps fmt referenced for future enrichment of fail messages.
var _ = fmt.Sprintf
