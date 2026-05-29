//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// SQL-CONTRACT COVERAGE for this file (ADR 0062 — adapter integration
// tests are SQL-contract-only; business-rule + state-machine coverage
// lives in tenanttest.FakeRepository unit tests):
//
//   - Outbox-row insertion in the same transaction as the tenant row on
//     both Add (identity.tenant_registered.v1) and UpdateByID/Activate
//     (identity.tenant_activated.v1). Verified subscriber-side after the
//     OutboxForwarder drains — strict TDL canon per ADR 0062 Amendment 1.
//     (Watermill-sql forwarder canon: same code path as production.)
//   - SQLSTATE 23505 → tenant.ErrSlugTaken translation via the unique
//     index on tenants.slug.
//
// PARALLELISM POLICY: tests that read outbox emissions via the
// per-package subscriber fixture (newOutboxFixture) are SERIAL + use
// sharedPG.TruncateAll(t) — the forwarder reads across every tenant
// + parallel fixtures would race on FOR UPDATE SKIP LOCKED. Tests that
// only touch tenant-scoped rows stay t.Parallel() under RLS isolation.

package adapters_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Shared bootstrap (repoFixture / TestMain / migrations / role
// provisioning) lives in fixture_integration_test.go per the Brandur /
// TDL canon — ONE container per package, shared pool, per-test
// isolation via fresh tenant_id + RLS.

func newTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	id := tenant.ID(ids.NewV7().String())
	// UUIDv7's leading chars are timestamp-derived → tests called in rapid
	// succession would collide on a prefix slug. Use the trailing random
	// portion (last 8 chars).
	full := ids.NewV7().String()
	s, err := slug.New("acme-pharma-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, err := email.New("admin@acme.test")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	tn, err := tenant.New(id, s, "Acme Pharma Pvt Ltd", "Acme", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	return tn
}

// SQL-contract: Add writes the outbox row in the same tx as the tenant
// row. Verified end-to-end via the production forwarder + an in-process
// Watermill subscriber — same code path as production. The aggregate
// round-trip (slug / status) is covered by tenanttest.FakeRepository.
//
// arch-test:no-parallel — subscriber-fixture test; uses TruncateAll
// (strict-TDL outbox-observation rule per ADR 0062 Amendment 1).
func TestTenantRepository_Add_PersistsOutboxEventInSameTx(t *testing.T) {
	sharedPG.TruncateAll(t)
	fix := newOutboxFixture(t)
	repo := adapters.NewTenantRepository(fix.pool, pg.NewTransactor(fix.pool))

	tn := newTenant(t)
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("Add: %v", err)
	}

	msgs := fix.forwardAndWait(t, 1)
	got := eventTypes(msgs)
	if len(got) != 1 || got[0] != "identity.tenant_registered.v1" {
		t.Fatalf("event_types: got %v want [identity.tenant_registered.v1]", got)
	}
	if tid := msgs[0].Metadata.Get(messaging.HeaderTenantID); tid != tn.ID().String() {
		t.Fatalf("tenant_id header: got %q want %q", tid, tn.ID().String())
	}
}

// SQL-contract: SQLSTATE 23505 on the unique tenants.slug index is
// translated to tenant.ErrSlugTaken. Does not observe outbox emissions,
// so stays t.Parallel() under RLS isolation.
//
// arch-test:parallel-safe — shared pgtest container + fresh tenant_id
// per test (newTenant uses ids.NewV7); RLS isolates rows by tenant so
// the duplicate-slug collision is self-contained. No TruncateAll, no
// cross-tenant scan, no process-global mutation.
func TestTenantRepository_Add_DuplicateSlug_ReturnsErrSlugTaken(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	first := newTenant(t)
	if err := repo.Add(ctx, first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Build a second tenant with a colliding slug.
	id2 := tenant.ID(ids.NewV7().String())
	addr, _ := email.New("other@acme.test")
	dup, err := tenant.New(id2, first.Slug(), "Other Pharma Ltd", "Other", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New dup: %v", err)
	}

	err = repo.Add(ctx, dup)
	if !errors.Is(err, tenant.ErrSlugTaken) {
		t.Fatalf("expected ErrSlugTaken, got %v", err)
	}
}

// SQL-contract: UpdateByID also enqueues an outbox row (tenant_activated.v1)
// in the same tx as the row UPDATE. Watermill-sql two-row-per-mutation
// invariant for both INSERT and UPDATE paths — verified subscriber-side.
//
// arch-test:no-parallel — subscriber-fixture test; uses TruncateAll.
func TestTenantRepository_UpdateByID_PersistsActivatedOutboxEventInSameTx(t *testing.T) {
	sharedPG.TruncateAll(t)
	fix := newOutboxFixture(t)
	repo := adapters.NewTenantRepository(fix.pool, pg.NewTransactor(fix.pool))

	tn := newTenant(t)
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	err := repo.UpdateByID(t.Context(), tn.ID(), func(t2 *tenant.Tenant) (bool, error) {
		if err := t2.Activate(testNow); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	// Forwarder drains BOTH rows + subscriber receives them in (created_at,
	// id) order — registered before activated. Order comes from the
	// `id` UUIDv7 tiebreaker on the forwarder's SELECT, gated by
	// TestArch_OutboxSelectsOrderByMonotonicTiebreaker.
	msgs := fix.forwardAndWait(t, 2)
	got := eventTypes(msgs)
	want := []string{"identity.tenant_registered.v1", "identity.tenant_activated.v1"}
	if len(got) != len(want) {
		t.Fatalf("event_types len: got %v want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("event_types[%d]: got %q want %q", i, got[i], w)
		}
	}
}
