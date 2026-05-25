//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via tenancy.WithID(); RLS isolates
//   rows by tenant so parallel runs cannot see each others state.
//   Brandur "Postgres at scale" + TDL Wild Workouts canon: shared
//   infrastructure + per-test logical isolation = safe parallelism.

package adapters_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg/rlstest"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// TestKeysetMembershipsPage_UsesIndexUnderRLS verifies the canon
// discipline from ADR 0038: the cursor-paginated keyset query
// MUST plan as an Index Scan against idx_memberships_tenant_active_joined,
// not a Seq Scan + Filter. Even under RLS predicates.
//
// Test shape:
//
//  1. Bring up testcontainers Postgres + apply migrations.
//  2. Seed 200 active memberships in one tenant (enough rows that
//     the planner prefers index over seq scan).
//  3. Set RLS GUC to the test tenant.
//  4. Run EXPLAIN (FORMAT JSON) on the same keyset query the
//     ListActiveMembershipsInTenantPage sqlc query emits.
//  5. Assert the plan starts with "Index Scan" and references the
//     expected index name.
//
// If this test fails, the planner has either gained a regression
// OR the partial-index predicate became incompatible with the
// query predicate — both warrant a manual EXPLAIN review.
func TestKeysetMembershipsPage_UsesIndexUnderRLS(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	ctx := t.Context()

	// --- Seed -------------------------------------------------------------
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	addr, _ := email.New("admin@keyset-test.com")
	s, _ := slug.New("keyset-test-tenant")
	tn, _ := tenant.New(tenant.ID(ids.NewV7().String()), s, "Keyset Test Ltd", "Keyset", addr, testNow)
	if err := tenants.Add(ctx, tn); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	hash, _ := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=1$abcd$efgh")

	// 200 active memberships — well above the planner's seq-scan
	// threshold (~10-20 rows typical).
	const seedCount = 200
	for i := 0; i < seedCount; i++ {
		paddr, _ := email.New(personEmailFor(i))
		p, _ := person.New(person.ID(ids.NewV7().String()), paddr, "User", string(rune('A'+i%26)), hash, testNow)
		if err := persons.Add(ctx, p); err != nil {
			t.Fatalf("seed person %d: %v", i, err)
		}
		m, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn.ID(), membership.ID(""), testNow)
		ctxWithTenant := tenancy.WithID(ctx, tenancy.ID(tn.ID().String()))
		if err := memberships.Add(ctxWithTenant, m); err != nil {
			t.Fatalf("seed membership %d: %v", i, err)
		}
	}

	// ANALYZE so the planner has up-to-date statistics. Without it
	// fresh-seeded data may not yet be reflected in pg_stat.
	if _, err := pool.Exec(ctx, `ANALYZE identity.tenant_memberships`); err != nil {
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
	rlstest.SetSessionTenant(t, ctx, dbtx, tn.ID().String())

	// The same predicate shape sqlc emits in ListActiveMembershipsInTenantPage.
	// First-page sentinel cursor admits every row.
	const explainSQL = `
		EXPLAIN (FORMAT JSON, ANALYZE, BUFFERS)
		SELECT id, person_id, tenant_id, status, joined_at
		FROM   identity.tenant_memberships
		WHERE  status = 'active'
		AND    (joined_at, id) < ($1::timestamptz, $2::uuid)
		ORDER  BY joined_at DESC, id DESC
		LIMIT  51
	`
	sentinelTime := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	sentinelID := "ffffffff-ffff-ffff-ffff-ffffffffffff"

	var rawPlan []byte
	if err := dbtx.QueryRow(ctx, explainSQL, sentinelTime, sentinelID).Scan(&rawPlan); err != nil {
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
	if !strings.Contains(planText, "idx_memberships_tenant_active_joined") {
		t.Errorf("expected plan to reference idx_memberships_tenant_active_joined; got:\n%s", planText)
	}
	// Bonus: Seq Scan on tenant_memberships would be a regression.
	if strings.Contains(planText, `"Node Type": "Seq Scan"`) &&
		strings.Contains(planText, `"Relation Name": "tenant_memberships"`) {
		t.Errorf("planner fell back to Seq Scan on tenant_memberships:\n%s", planText)
	}

	// Also verify the JSON is well-formed (catches future planner
	// output-format changes early).
	var parsed []map[string]any
	if err := json.Unmarshal(rawPlan, &parsed); err != nil {
		t.Fatalf("plan JSON malformed: %v", err)
	}
}

// personEmailFor builds a unique email for each seeded test
// person — avoids the persons.email UNIQUE constraint blowing up.
func personEmailFor(i int) string {
	return "user" + string(rune('0'+i/100)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10)) + "@keyset.test"
}

// _ keeps the context import live even if future refactors collapse
// the test body.
var _ context.Context = context.Background() // arch-test:context-background — package-level type assertion, no *testing.T in scope
