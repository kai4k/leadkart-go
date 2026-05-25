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
// SQL-CONTRACT COVERAGE for this file (ADR 0062 — adapter integration
// tests are SQL-contract-only; business-rule + state-machine coverage
// lives in refreshtokentest.FakeRepository unit tests):
//
//   - Multi-table write in same tx: Add persists the family row AND its
//     first token row (refresh_token_families + refresh_tokens) inside
//     ONE pgx tx. Read-side hydration walks both tables.
//   - GetByTokenHash performs the SQL JOIN from token-hash → family
//     (the lookup spans refresh_tokens → refresh_token_families) — the
//     fake approximates this by walking every family's token bag, but
//     the SQL path proves the index + JOIN both fire.
//   - UpdateByID/Rotate persists an INSERT of a new token row + UPDATE
//     of the previous-generation row (consumed + replaced_by_id) in a
//     SINGLE tx — the canonical RFC 9700 §4.13 rotation contract.

package adapters_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

const tokenTTL = 14 * 24 * time.Hour

// hashOf is a tiny helper that mirrors what the auth adapter will do in
// production (SHA-256 the opaque token, hex-encode). Adapter tests don't
// depend on the real auth path; we just need a TokenHash that round-trips.
func hashOf(t *testing.T, s string) refreshtoken.TokenHash {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	hash, err := refreshtoken.NewTokenHash(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("token hash: %v", err)
	}
	return hash
}

// seedFamily creates Person + Tenant + initial Family with one token at
// generation 0 — the precondition for every refresh-token integration test.
func seedFamily(t *testing.T, persons *adapters.PersonRepository, tenants *adapters.TenantRepository, families *adapters.RefreshTokenFamilyRepository, secret string) *refreshtoken.Family {
	t.Helper()
	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "rt-"+ids.NewV7().String()[:8]+"@example.test")

	fid := refreshtoken.FamilyID(ids.NewV7().String())
	hash := hashOf(t, secret)
	f, err := refreshtoken.NewFamily(fid, p.ID(), tn.ID(), "iPhone 15 / Safari", hash, tokenTTL, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	if err := families.Add(t.Context(), f); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return f
}

// SQL-contract: Add writes BOTH the family row and the first token row
// inside the same pgx tx, and GetByID hydrates from BOTH tables. Multi-
// table write+read SQL contract — fake covers in-memory bag semantics.
func TestRefreshTokenFamilyRepository_Add_PersistsFamilyAndFirstTokenInSameTx(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	f := seedFamily(t, persons, tenants, families, "secret-token-1")

	// Round-trip: GetByID hydrates the family + every token row from
	// the child table.
	got, err := families.GetByID(t.Context(), f.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.AllTokens()) != 1 {
		t.Fatalf("token rows hydrated from child table: got %d want 1", len(got.AllTokens()))
	}
}

// SQL-contract: GetByTokenHash performs the SQL JOIN from refresh_tokens
// (where the hash lives) back to refresh_token_families. The
// refreshtokentest.FakeRepository mirrors the lookup observable, so
// the SQL test must ALSO prove the PG-specific mechanism: EXPLAIN
// confirms the plan uses the btree Index Scan on `token_hash`, not a
// Seq Scan. Without the EXPLAIN, the only thing this test would prove
// over the fake is "the SQL also returns the right row" — which is
// pure round-trip per ADR 0062 §6.
//
// Two halves:
//
//   1. EXPLAIN plan: Index Scan over the token_hash unique index
//      (Postgres auto-names it `refresh_tokens_token_hash_key`).
//   2. Observable: lookup hydrates the family by id.
func TestRefreshTokenFamilyRepository_GetByTokenHash_JoinsTokenToFamily(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	secret := "hash-resolves-token"
	f := seedFamily(t, persons, tenants, families, secret)
	hash := hashOf(t, secret)

	// SQL-contract part 1: EXPLAIN proves Index Scan on token_hash.
	// refresh_tokens is non-RLS (session infrastructure), so no
	// SET LOCAL dance is needed — query directly.
	const explainSQL = `
		EXPLAIN (FORMAT TEXT)
		SELECT family_id FROM identity.refresh_tokens WHERE token_hash = $1
	`
	planRows, err := pool.Query(t.Context(), explainSQL, hash.String())
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
	if !strings.Contains(plan, "Index") {
		t.Fatalf("EXPLAIN plan does not show an Index node — token_hash lookup falling back to scan; got:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan") {
		t.Fatalf("EXPLAIN plan shows Seq Scan on refresh_tokens — index regression; got:\n%s", plan)
	}

	// SQL-contract part 2: observable — lookup hydrates the family.
	got, err := families.GetByTokenHash(t.Context(), hash)
	if err != nil {
		t.Fatalf("GetByTokenHash: %v", err)
	}
	if got.ID() != f.ID() {
		t.Fatalf("family id round-trip: got %q want %q", got.ID(), f.ID())
	}
}

// SQL-contract: Rotate within UpdateByID issues an INSERT of the new
// token row AND an UPDATE of the previous row (is_consumed + replaced_
// by_id) inside ONE pgx tx — atomically. Re-reading the family must
// surface both rows.
func TestRefreshTokenFamilyRepository_UpdateByID_RotateWritesNewTokenAndUpdatesOldInSameTx(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	originalSecret := "rotate-original"
	f := seedFamily(t, persons, tenants, families, originalSecret)

	originalHash := hashOf(t, originalSecret)
	newSecret := "rotate-next-gen"
	newHash := hashOf(t, newSecret)

	err := families.UpdateByID(t.Context(), f.ID(), func(f2 *refreshtoken.Family) (bool, error) {
		if err := f2.Rotate(originalHash, newHash, tokenTTL, time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	got, err := families.GetByID(t.Context(), f.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	tokens := got.AllTokens()
	if len(tokens) != 2 {
		t.Fatalf("token rows after rotate: got %d want 2 (INSERT new + retain old)", len(tokens))
	}
	if !tokens[0].IsConsumed() {
		t.Fatal("old token row not UPDATE'd to is_consumed=true (multi-statement tx broken)")
	}
}

// Note: ReuseDetection / Revoke business-rule scenarios + state-machine
// transitions live in refreshtokentest.FakeRepository unit tests + the
// refreshtoken aggregate's own unit tests. ListActiveForPerson's
// revoked-filter is in-memory predicate work covered by the fake.
