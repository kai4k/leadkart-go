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
// lives in tenanttest.FakeRepository unit tests):
//
//   - Outbox-row insertion in the same transaction as the tenant row on
//     both Add (identity.tenant_registered.v1) and UpdateByID/Activate
//     (identity.tenant_activated.v1). Watermill-sql forwarder canon.
//   - SQLSTATE 23505 → tenant.ErrSlugTaken translation via the unique
//     index on tenants.slug.

package adapters_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
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
// row. The aggregate round-trip (slug / status) is covered by
// tenanttest.FakeRepository.
func TestTenantRepository_Add_PersistsOutboxEventInSameTx(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	tn := newTenant(t)
	if err := repo.Add(ctx, tn); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Outbox row written with topic identity.tenant_registered.v1.
	// Outbox is RLS+FORCE — the helper bypasses the policy internally
	// (mirrors what the Watermill forwarder does in production).
	topics := messagingtest.OutboxTopicsForTenant(t, pool, messagingtest.SchemaIdentity, tn.ID().String())
	if len(topics) == 0 {
		t.Fatal("read outbox: no rows")
	}
	if topics[0] != "identity.tenant_registered.v1" {
		t.Fatalf("outbox topic: got %q want identity.tenant_registered.v1", topics[0])
	}
}

// SQL-contract: SQLSTATE 23505 on the unique tenants.slug index is
// translated to tenant.ErrSlugTaken.
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
// invariant for both INSERT and UPDATE paths.
func TestTenantRepository_UpdateByID_PersistsActivatedOutboxEventInSameTx(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	tn := newTenant(t)
	if err := repo.Add(ctx, tn); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	// Activate via UpdateFn closure.
	err := repo.UpdateByID(ctx, tn.ID(), func(t2 *tenant.Tenant) (bool, error) {
		if err := t2.Activate(testNow); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	// Outbox now has both registered + activated events for this tenant.
	topics := messagingtest.OutboxTopicsForTenant(t, pool, messagingtest.SchemaIdentity, tn.ID().String())
	want := []string{"identity.tenant_registered.v1", "identity.tenant_activated.v1"}
	if len(topics) != len(want) {
		t.Fatalf("outbox topics: got %v want %v", topics, want)
	}
	for i, w := range want {
		if topics[i] != w {
			t.Fatalf("outbox[%d]: got %q want %q", i, topics[i], w)
		}
	}
}
