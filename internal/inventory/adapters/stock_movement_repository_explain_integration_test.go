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
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// TestKeysetStockMovementsPage_UsesIndexUnderRLS asserts the per-batch
// keyset query plans as an Index Scan against idx_movements_batch_keyset
// (batch_id, occurred_at DESC, id DESC) — ADR 0038 + migration 20260603000001.
// Seeds 200 inbound movements so the planner has enough rows to prefer the index.
func TestKeysetStockMovementsPage_UsesIndexUnderRLS(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	movements := adapters.NewStockMovementRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	p, err := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "MOV-EXPL", Name: "MovExpl", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	if err != nil {
		t.Fatalf("product.New: %v", err)
	}
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add product: %v", err)
	}

	b, err := batch.New(batch.ID(ids.NewV7().String()), p.ID(), tid, actor,
		batch.Spec{
			BatchNumber:                "LOT-MOV",
			ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			ExpiryDate:                 time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
			ManufacturerName:           "Acme",
			ManufacturingLicenceNumber: "ML-1",
			MRPPaise:                   100,
			PurchasePricePaise:         50,
		}, fixedNow)
	if err != nil {
		t.Fatalf("batch.New: %v", err)
	}
	if err := batches.Add(ctx, b); err != nil {
		t.Fatalf("Add batch: %v", err)
	}

	const seedCount = 200
	for i := range seedCount {
		// Distinct occurred_at per row keeps the keyset index discriminating.
		occurredAt := fixedNow.Add(time.Duration(i) * time.Second)
		err := tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
			// Reload + apply under one tx: version check and outbox write stay consistent.
			return batches.UpdateByID(ctx, tid, b.ID(), func(bb *batch.Batch) (bool, error) {
				if err := bb.ApplyMovement(batch.MovementInbound, 1, occurredAt); err != nil {
					return false, err
				}
				m, err := stockmovement.New(
					stockmovement.ID(ids.NewV7().String()),
					stockmovement.Spec{
						BatchID:             b.ID(),
						ProductID:           p.ID(),
						TenantID:            tid,
						Type:                batch.MovementInbound,
						Quantity:            1,
						QuantityOnHandAfter: bb.QuantityOnHand(),
						Reason:              fmt.Sprintf("seed-%d", i),
						ActorMembershipID:   actor,
					}, occurredAt)
				if err != nil {
					return false, err
				}
				if err := movements.Add(ctx, m); err != nil {
					return false, err
				}
				return true, nil
			})
		})
		if err != nil {
			t.Fatalf("seed movement %d: %v", i, err)
		}
	}

	if _, err := pool.Exec(ctx, `ANALYZE inventory.stock_movements`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	rlstest.SetSessionTenantPersistent(t, ctx, conn.Conn(), tid.String())

	// Same predicate shape as sqlc's ListMovementsByBatchPage.
	const explainSQL = `
		EXPLAIN (FORMAT JSON, ANALYZE, BUFFERS)
		SELECT id, batch_id, product_id, tenant_id,
		       type, quantity, quantity_on_hand_after,
		       reason, actor_membership_id, source_reference,
		       occurred_at
		FROM   inventory.stock_movements
		WHERE  batch_id = $1::uuid
		AND    ($4::text = '' OR type = $4::text)
		AND    (occurred_at, id) < ($2::timestamptz, $3::uuid)
		ORDER  BY occurred_at DESC, id DESC
		LIMIT  51
	`

	// Mirror of internal/inventory/adapters/cursor.go sentinels.
	sentinelTime := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	sentinelID := "ffffffff-ffff-ffff-ffff-ffffffffffff"

	var rawPlan []byte
	if err := conn.QueryRow(ctx, explainSQL,
		b.ID().String(), sentinelTime, sentinelID, "",
	).Scan(&rawPlan); err != nil {
		t.Fatalf("explain: %v", err)
	}

	planText := string(rawPlan)
	t.Logf("EXPLAIN plan:\n%s", planText)

	if !strings.Contains(planText, "Index Scan") &&
		!strings.Contains(planText, "Bitmap Index Scan") {
		t.Errorf("expected plan to contain Index Scan (any flavour); got:\n%s", planText)
	}
	if strings.Contains(planText, `"Node Type": "Seq Scan"`) &&
		strings.Contains(planText, `"Relation Name": "stock_movements"`) {
		t.Errorf("planner fell back to Seq Scan on inventory.stock_movements:\n%s", planText)
	}

	var parsed []map[string]any
	if err := json.Unmarshal(rawPlan, &parsed); err != nil {
		t.Fatalf("plan JSON malformed: %v", err)
	}
}
