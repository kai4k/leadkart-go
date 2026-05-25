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

func TestTenantRepository_Add_PersistsRowAndOutboxEvent(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	tn := newTenant(t)
	if err := repo.Add(ctx, tn); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Round-trip: GetByID returns the same logical tenant.
	got, err := repo.GetByID(ctx, tn.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Slug() != tn.Slug() {
		t.Fatalf("slug round-trip: got %q want %q", got.Slug(), tn.Slug())
	}
	if got.Status() != tenant.StatusPending {
		t.Fatalf("status round-trip: got %v want %v", got.Status(), tenant.StatusPending)
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

func TestTenantRepository_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	missing := tenant.ID(ids.NewV7().String())
	_, err := repo.GetByID(ctx, missing)
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestTenantRepository_UpdateByID_ActivatesAndDrainsEvent(t *testing.T) {
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

	got, err := repo.GetByID(ctx, tn.ID())
	if err != nil {
		t.Fatalf("GetByID after activate: %v", err)
	}
	if got.Status() != tenant.StatusActive {
		t.Fatalf("expected active, got %v", got.Status())
	}
	if got.ActivatedAt().IsZero() {
		t.Fatal("activated_at not set")
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

func TestTenantRepository_UpdateByID_NoOpClosureSkipsPersist(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	tn := newTenant(t)
	if err := repo.Add(ctx, tn); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	// Closure returns (false, nil) — skip persist + skip events.
	err := repo.UpdateByID(ctx, tn.ID(), func(_ *tenant.Tenant) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := repo.GetByID(ctx, tn.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status() != tenant.StatusPending {
		t.Fatalf("expected unchanged status pending, got %v", got.Status())
	}
}

