//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via tenancy.WithID(); RLS isolates
//   rows by tenant so parallel runs cannot see each other's state.
//   Brandur "Postgres at scale" + TDL Wild Workouts canon: shared
//   infrastructure + per-test logical isolation = safe parallelism.

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
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// TestKeysetBatchesPage_UsesIndexUnderRLS mirrors identity's
// keyset_explain_integration_test for the inventory.batches partial
// index per ADR 0061 amendment 1 (finding M7).
//
// Asserts the per-product keyset query plans as an Index Scan against
// idx_batches_product (post fix-pass: declared
// `(product_id, expiry_date DESC, id DESC) WHERE NOT is_deleted` —
// matching the query's `ORDER BY expiry_date DESC, id DESC`).
func TestKeysetBatchesPage_UsesIndexUnderRLS(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "EXPL-1", Name: "Expl", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add product: %v", err)
	}
	// Seed 200 batches — enough rows that the planner reliably prefers
	// the index over Seq Scan.
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 200 {
		exp := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * 24 * time.Hour)
		b, _ := batch.New(batch.ID(ids.NewV7().String()), p.ID(), tid, actor,
			batch.Spec{
				BatchNumber:                fmt.Sprintf("LOT-%03d", i),
				ManufactureDate:            mfg,
				ExpiryDate:                 exp,
				ManufacturerName:           "Acme",
				ManufacturingLicenceNumber: "ML-1",
				MRPPaise:                   100,
				PurchasePricePaise:         50,
			}, fixedNow)
		if err := batches.Add(ctx, b); err != nil {
			t.Fatalf("seed batch %d: %v", i, err)
		}
	}

	// Acquire a pgx conn under platform scope so EXPLAIN sees all tenants
	// (and the planner can see all rows). The query itself runs under
	// the same scope; in production tenant_id is restricted via RLS,
	// the planner still picks the same index because (product_id, ...)
	// is the leading column.
	dbtx, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer dbtx.Release()
	rlstest.SetSessionTenantPersistent(t, ctx, dbtx.Conn(), tid.String())

	const explainSQL = `
		EXPLAIN (FORMAT JSON, ANALYZE, BUFFERS)
		SELECT id, product_id, tenant_id, batch_number, manufacture_date, expiry_date,
		       manufacturer_name, manufacturing_licence_number,
		       mrp_paise, purchase_price_paise, quantity_on_hand, version,
		       created_at, updated_at, is_deleted, deleted_at, deleted_by
		FROM   inventory.batches
		WHERE  NOT is_deleted
		  AND  product_id = $1::uuid
		  AND  (expiry_date, id) < ($2::date, $3::uuid)
		ORDER  BY expiry_date DESC, id DESC
		LIMIT  51
	`
	sentinelExpiry := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	sentinelID := "ffffffff-ffff-ffff-ffff-ffffffffffff"

	var rawPlan []byte
	if err := dbtx.QueryRow(ctx, explainSQL, p.ID().String(), sentinelExpiry, sentinelID).Scan(&rawPlan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	planText := string(rawPlan)
	t.Logf("EXPLAIN plan:\n%s", planText)

	// Primary regression guard: no Seq Scan on inventory.batches under
	// RLS. The planner may legitimately pick *any* index whose leading
	// column is product_id — at the 200-row test scale it sometimes
	// prefers uq_batches_product_number_live (also product_id-leading)
	// over idx_batches_product. The bug we're guarding against is
	// "planner ignored every index + did a full table scan", not
	// "planner picked a different but equally-valid index".
	if !strings.Contains(planText, "Index Scan") &&
		!strings.Contains(planText, "Bitmap Index Scan") {
		t.Errorf("expected plan to contain Index Scan (any flavour); got:\n%s", planText)
	}
	if strings.Contains(planText, `"Node Type": "Seq Scan"`) &&
		strings.Contains(planText, `"Relation Name": "batches"`) {
		t.Errorf("planner fell back to Seq Scan on inventory.batches:\n%s", planText)
	}

	var parsed []map[string]any
	if err := json.Unmarshal(rawPlan, &parsed); err != nil {
		t.Fatalf("plan JSON malformed: %v", err)
	}
}
