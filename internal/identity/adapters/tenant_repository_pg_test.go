//go:build integration

package adapters_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// repoFixture spins an ephemeral Postgres + applies migrations + returns
// the pgxpool. Container is auto-cleaned via t.Cleanup. Test connects as
// the (default) superuser — the tenants + outbox tables we touch don't
// require RLS bypass for inserts since this repository runs under
// platform scope by design.
func repoFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("leadkart_test"),
		postgres.WithUsername("leadkart"),
		postgres.WithPassword("leadkart_test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	// Apply migrations via the goose+database/sql path.
	gooseDB, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("goose open: %v", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, gooseDB, migrationsDir(t)); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("goose up: %v", err)
	}
	_ = gooseDB.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// here = .../internal/identity/adapters/tenant_repository_pg_test.go
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "migrations")
}

func newTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	id := tenant.ID(ids.NewV7().String())
	s, err := slug.New("acme-pharma-" + ids.NewV7().String()[:8])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, err := email.New("admin@acme.test")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	tn, err := tenant.New(id, s, "Acme Pharma Pvt Ltd", "Acme", addr)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	return tn
}

func TestTenantRepository_Add_PersistsRowAndOutboxEvent(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := context.Background()

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
	var topic string
	rawDB, err := openRawDB(t, pool)
	if err != nil {
		t.Fatalf("openRawDB: %v", err)
	}
	defer rawDB.Close()
	if err := rawDB.QueryRowContext(ctx, `
		SELECT topic FROM identity.outbox WHERE tenant_id = $1
	`, tn.ID().String()).Scan(&topic); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if topic != "identity.tenant_registered.v1" {
		t.Fatalf("outbox topic: got %q want identity.tenant_registered.v1", topic)
	}
}

func TestTenantRepository_Add_DuplicateSlug_ReturnsErrSlugTaken(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := context.Background()

	first := newTenant(t)
	if err := repo.Add(ctx, first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Build a second tenant with a colliding slug.
	id2 := tenant.ID(ids.NewV7().String())
	addr, _ := email.New("other@acme.test")
	dup, err := tenant.New(id2, first.Slug(), "Other Pharma Ltd", "Other", addr)
	if err != nil {
		t.Fatalf("tenant.New dup: %v", err)
	}

	err = repo.Add(ctx, dup)
	if !errors.Is(err, tenant.ErrSlugTaken) {
		t.Fatalf("expected ErrSlugTaken, got %v", err)
	}
}

func TestTenantRepository_GetByID_NotFound(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := context.Background()

	missing := tenant.ID(ids.NewV7().String())
	_, err := repo.GetByID(ctx, missing)
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestTenantRepository_UpdateByID_ActivatesAndDrainsEvent(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := context.Background()

	tn := newTenant(t)
	if err := repo.Add(ctx, tn); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	// Activate via UpdateFn closure.
	err := repo.UpdateByID(ctx, tn.ID(), func(t2 *tenant.Tenant) (bool, error) {
		if err := t2.Activate(); err != nil {
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
	rawDB, err := openRawDB(t, pool)
	if err != nil {
		t.Fatalf("openRawDB: %v", err)
	}
	defer rawDB.Close()
	rows, err := rawDB.QueryContext(ctx, `
		SELECT topic FROM identity.outbox WHERE tenant_id = $1 ORDER BY occurred_at
	`, tn.ID().String())
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()

	var topics []string
	for rows.Next() {
		var topic string
		if err := rows.Scan(&topic); err != nil {
			t.Fatalf("scan: %v", err)
		}
		topics = append(topics, topic)
	}
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
	pool := repoFixture(t)
	repo := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	ctx := context.Background()

	tn := newTenant(t)
	if err := repo.Add(ctx, tn); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	// Closure returns (false, nil) — skip persist + skip events.
	err := repo.UpdateByID(ctx, tn.ID(), func(t2 *tenant.Tenant) (bool, error) {
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

// openRawDB returns a database/sql handle pointed at the same DSN as the
// pgxpool — used for verification queries that bypass the repository.
func openRawDB(t *testing.T, pool *pgxpool.Pool) (*sql.DB, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := cfg.ConnString()
	if dsn == "" {
		return nil, errors.New("pool has no ConnString")
	}
	return sql.Open("pgx", dsn)
}
