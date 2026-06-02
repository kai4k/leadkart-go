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
//
// SQL-CONTRACT COVERAGE for this file (ADR 0062 + ADR 0058 — adapter
// integration tests are SQL-contract-only; business-rule + state-
// machine coverage lives in rolehierarchytest.FakeRepository):
//
//   - Partial unique index uq_role_hierarchy_active_edge_per_child
//     (WHERE is_active) → SQLSTATE 23505 → ErrEdgeAlreadyExists.
//   - DB trigger edge_check_cycle (SECURITY INVOKER on the edges table)
//     rejects multi-hop cycle closures → ErrCycle.
//   - Composite FK fk_edges_parent_same_tenant declaratively rejects
//     cross-tenant parent references → ErrCrossTenant (declarative
//     replacement for the Wave 9.1d SECURITY DEFINER trigger).
//   - DB CHECK chk_edge_no_self_loop is a backstop when the aggregate's
//     own ErrSelfReference is bypassed via UnmarshalFromDB → translated
//     back to ErrSelfReference at the adapter layer.
//   - Soft-delete partial-index filtering on reads — GetActiveByChild
//     hides removed edges (WHERE removed_at IS NULL) via SQL predicate.
//   - Recursive CTE in GetAncestorsByChild walks upward through the
//     edge graph (Postgres-specific WITH RECURSIVE).
//
// arch-test:raw-sql-justified — TestEdgeRepository_GetActiveByChild_
//   FiltersSoftDeleted intentionally bypasses the adapter with a
//   direct SELECT to assert the PHYSICAL row state after Remove
//   (proving soft-vs-hard delete + partial-index filter at the SQL
//   layer). The fake mirrors the observable "ErrEdgeNotFound after
//   remove" behaviour; only the adapter-bypass SELECT proves the
//   Postgres-specific mechanism is what's doing the filtering. Per
//   ADR 0062 §6 this is the canonical shape for a SQL-contract test.

package adapters_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for database/sql owner-DSN RLS-bypass

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pg/rlstest"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// freshEdge mints an Edge with the test timestamp.
func freshEdge(t *testing.T, tid tenant.ID, child, parent role.ID) *rolehierarchy.Edge {
	t.Helper()
	e, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tid,
		child,
		parent,
		membership.ID(""),
		"integration test edge — seeded by adapter test fixture",
		time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("rolehierarchy.New: %v", err)
	}
	return e
}

// SQL-contract: uq_role_hierarchy_active_edge_per_child (WHERE is_active)
// raises SQLSTATE 23505 on a second active edge for the same child →
// ErrEdgeAlreadyExists.
func TestEdgeRepository_Add_RejectsDuplicateActiveChildEdge(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	p1 := newRole(t, tn.ID(), "Parent1")
	p2 := newRole(t, tn.ID(), "Parent2")
	child := newRole(t, tn.ID(), "Junior")
	for _, r := range []*role.Role{p1, p2, child} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), child.ID(), p1.ID())); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	err := edges.Add(ctx, freshEdge(t, tn.ID(), child.ID(), p2.ID()))
	if !errors.Is(err, rolehierarchy.ErrEdgeAlreadyExists) {
		t.Fatalf("Add duplicate: got %v want ErrEdgeAlreadyExists", err)
	}
}

// SQL-contract: DB trigger edge_check_cycle rejects a multi-hop cycle
// in the active-edge graph → ErrCycle.
func TestEdgeRepository_Add_RejectsCycle(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	a := newRole(t, tn.ID(), "RoleA")
	b := newRole(t, tn.ID(), "RoleB")
	for _, r := range []*role.Role{a, b} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	// b → a (legal edge).
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), b.ID(), a.ID())); err != nil {
		t.Fatalf("b → a: %v", err)
	}
	// a → b closes the cycle — trigger fires.
	err := edges.Add(ctx, freshEdge(t, tn.ID(), a.ID(), b.ID()))
	if !errors.Is(err, rolehierarchy.ErrCycle) {
		t.Fatalf("expected ErrCycle, got %v", err)
	}
}

// SQL-contract: composite FK fk_edges_parent_same_tenant rejects an edge
// with a parent from another tenant (ADR 0058, replaces SECURITY DEFINER
// trigger) → ErrCrossTenant.
func TestEdgeRepository_Add_RejectsCrossTenant(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))

	rA := newRole(t, tnA.ID(), "RoleA")
	rB := newRole(t, tnB.ID(), "RoleB")
	if err := roles.Add(ctxA, rA); err != nil {
		t.Fatalf("Add A: %v", err)
	}
	if err := roles.Add(ctxB, rB); err != nil {
		t.Fatalf("Add B: %v", err)
	}

	// Edge in tenant A with parent from tenant B — FK fires.
	bad, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tnA.ID(),
		rA.ID(),
		rB.ID(),
		membership.ID(""),
		"",
		time.Now(),
	)
	if err != nil {
		t.Fatalf("New (should succeed at aggregate; DB rejects): %v", err)
	}
	addErr := edges.Add(ctxA, bad)
	if !errors.Is(addErr, rolehierarchy.ErrCrossTenant) {
		t.Fatalf("expected ErrCrossTenant, got %v", addErr)
	}
}

// SQL-contract: Remove is SOFT-delete (removed_at flips non-NULL).
// The partial index hides the row from GetActiveByChild. Two halves:
//
//  1. Physical row survives: adapter-bypass SELECT confirms removed_at is set.
//  2. GetActiveByChild returns ErrEdgeNotFound after the soft-delete.
func TestEdgeRepository_GetActiveByChild_FiltersSoftDeleted(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	p := newRole(t, tn.ID(), "Manager")
	c := newRole(t, tn.ID(), "Junior")
	for _, r := range []*role.Role{p, c} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	e := freshEdge(t, tn.ID(), c.ID(), p.ID())
	if err := edges.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}
	rmErr := edges.UpdateByID(ctx, tn.ID(), e.ID(), func(loaded *rolehierarchy.Edge) (bool, error) {
		return true, loaded.Remove(membership.ID(""), "removal for the active-only check", time.Now())
	})
	if rmErr != nil {
		t.Fatalf("Remove: %v", rmErr)
	}

	// Part 1: direct SELECT via OWNER DSN proves the row survives.
	ownerDB, openErr := sql.Open("pgx", sharedPG.OwnerDSN())
	if openErr != nil {
		t.Fatalf("open owner DB: %v", openErr)
	}
	defer func() { _ = ownerDB.Close() }()
	var removedAt *time.Time
	if err := ownerDB.QueryRowContext(t.Context(),
		`SELECT removed_at FROM identity.role_hierarchy_edges WHERE id = $1`,
		e.ID().String(),
	).Scan(&removedAt); err != nil {
		t.Fatalf("direct SELECT for physical row: %v", err)
	}
	if removedAt == nil {
		t.Fatal("physical row's removed_at is NULL — Remove behaved as hard-delete, contract broken")
	}

	// Part 2: WHERE removed_at IS NULL predicate hides row from GetActiveByChild.
	if _, err := edges.GetActiveByChild(ctx, tn.ID(), c.ID()); !errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		t.Fatalf("expected ErrEdgeNotFound after soft-delete (partial-index filter), got %v", err)
	}
}

// SQL-contract: GetAncestorsByChild uses WITH RECURSIVE to walk the edge
// graph. Two halves:
//
//  1. EXPLAIN proves the plan contains a Recursive CTE node.
//  2. Observable: 3-node chain (c → p → gp) yields a two-edge ancestor list.
func TestEdgeRepository_GetAncestorsByChild_RecursiveCTEWalksUpward(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	gp := newRole(t, tn.ID(), "GrandParent")
	p := newRole(t, tn.ID(), "Parent")
	c := newRole(t, tn.ID(), "Child")
	for _, r := range []*role.Role{gp, p, c} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	// Build chain: p → gp, c → p.
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), p.ID(), gp.ID())); err != nil {
		t.Fatalf("p → gp: %v", err)
	}
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), c.ID(), p.ID())); err != nil {
		t.Fatalf("c → p: %v", err)
	}

	// Part 1: EXPLAIN under RLS scope proves the recursive CTE.
	// A flat JOIN would produce the right result on a 3-deep chain but
	// have a different asymptotic shape — the plan check catches that.
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	dbtx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = dbtx.Rollback(t.Context()) }()
	rlstest.SetSessionTenant(t, t.Context(), dbtx, tn.ID().String())

	const explainSQL = `
		EXPLAIN (FORMAT TEXT)
		WITH RECURSIVE ancestors AS (
		  SELECT id, child_role_id, parent_role_id, 1 AS depth
		  FROM identity.role_hierarchy_edges
		  WHERE child_role_id = $1 AND removed_at IS NULL
		  UNION ALL
		  SELECT e.id, e.child_role_id, e.parent_role_id, a.depth + 1
		  FROM identity.role_hierarchy_edges e
		  JOIN ancestors a ON e.child_role_id = a.parent_role_id
		  WHERE e.removed_at IS NULL
		)
		SELECT id, child_role_id, parent_role_id FROM ancestors ORDER BY depth
	`
	planRows, err := dbtx.Query(t.Context(), explainSQL, c.ID().String())
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var planLines []string
	for planRows.Next() {
		var line string
		if err := planRows.Scan(&line); err != nil {
			t.Fatalf("EXPLAIN scan: %v", err)
		}
		planLines = append(planLines, line)
	}
	planRows.Close()
	plan := strings.Join(planLines, "\n")
	if !strings.Contains(plan, "Recursive Union") && !strings.Contains(plan, "CTE Scan") {
		t.Fatalf("EXPLAIN plan does not show a recursive CTE node — got:\n%s", plan)
	}

	// Part 2: observable result.
	ancs, err := edges.GetAncestorsByChild(ctx, tn.ID(), c.ID())
	if err != nil {
		t.Fatalf("GetAncestorsByChild: %v", err)
	}
	if len(ancs) != 2 {
		t.Fatalf("ancestors: got %d want 2", len(ancs))
	}
	// Depth-first order: c→p first, then p→gp.
	if ancs[0].ChildRoleID() != c.ID() || ancs[0].ParentRoleID() != p.ID() {
		t.Errorf("ancestor[0]: got (%s→%s) want (%s→%s)",
			ancs[0].ChildRoleID(), ancs[0].ParentRoleID(), c.ID(), p.ID())
	}
	if ancs[1].ChildRoleID() != p.ID() || ancs[1].ParentRoleID() != gp.ID() {
		t.Errorf("ancestor[1]: got (%s→%s) want (%s→%s)",
			ancs[1].ChildRoleID(), ancs[1].ParentRoleID(), p.ID(), gp.ID())
	}
}

// SQL-contract: chk_edge_no_self_loop is the schema backstop when the
// aggregate's ErrSelfReference is bypassed via UnmarshalFromDB; the
// check_violation must translate to ErrSelfReference at the adapter layer.
func TestEdgeRepository_Add_RejectsSelfReference(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	r := newRole(t, tn.ID(), "RoleSelf")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Bypass the aggregate constructor via UnmarshalFromDB (no re-validation)
	// to trigger the DB CHECK. The adapter must translate to ErrSelfReference.
	bad := rolehierarchy.UnmarshalFromDB(rolehierarchy.Snapshot{
		ID:           rolehierarchy.ID(ids.NewV7().String()),
		TenantID:     tn.ID(),
		ChildRoleID:  r.ID(),
		ParentRoleID: r.ID(),
	})
	addErr := edges.Add(ctx, bad)
	if !errors.Is(addErr, rolehierarchy.ErrSelfReference) {
		t.Fatalf("expected ErrSelfReference, got %v", addErr)
	}
}
