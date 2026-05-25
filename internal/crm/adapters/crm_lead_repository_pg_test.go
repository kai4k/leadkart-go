//go:build integration

package adapters_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/crm/adapters"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// crmRepoFixture mirrors the identity-side repoFixture: spins an
// ephemeral Postgres, applies migrations as owner, then provisions the
// non-superuser leadkart_app role + grants on the crm schema (and the
// app + identity schemas, since the migration tree creates them too).
func crmRepoFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
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

	ownerDSN, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if err := bootstrapTestDB(ctx, ownerDSN, migrationsDir(t)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	host, port, err := containerHostPort(ctx, c)
	if err != nil {
		t.Fatalf("host:port: %v", err)
	}
	appDSN := "postgres://leadkart_app:leadkart_app_pw@" + host + ":" + port + "/leadkart_test?sslmode=disable"

	pool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func bootstrapTestDB(ctx context.Context, ownerDSN, dir string) error {
	gooseDB, err := goose.OpenDBWithDriver("pgx", ownerDSN)
	if err != nil {
		return fmt.Errorf("goose open: %w", err)
	}
	defer gooseDB.Close()
	// Use the race-safe wrapper (see internal/common/pg/goose_helper.go):
	// goose.SetDialect mutates a package global; parallel testcontainers
	// callers race on it. EnsureGooseDialect serialises via sync.Once.
	if err := pg.EnsureGooseDialect(); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, gooseDB, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	// Provision leadkart_app + grants on every product schema. The
	// identity / platform / inventory schemas may or may not exist at
	// the time of this run depending on which sibling migrations are
	// present; the schema list mirrors what production grants once each
	// module ships its first migration.
	stmts := []string{
		`CREATE ROLE leadkart_app LOGIN PASSWORD 'leadkart_app_pw' NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
		`GRANT USAGE ON SCHEMA app, identity, crm TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA crm TO leadkart_app`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO leadkart_app`,
	}
	for _, s := range stmts {
		if _, err := gooseDB.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("provision: %s: %w", s, err)
		}
	}
	return nil
}

func containerHostPort(ctx context.Context, c *postgres.PostgresContainer) (string, string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", "", fmt.Errorf("host: %w", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return "", "", fmt.Errorf("port: %w", err)
	}
	return host, port.Port(), nil
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "migrations")
}

// withTenantCtx attaches a tenant ID to the context so the repository's
// tenant-scoped reads have a target tenant. Mirrors what the HTTP
// middleware does in production.
func withTenantCtx(ctx context.Context, tenantID uuid.UUID) context.Context {
	return tenancy.WithID(ctx, tenancy.ID(tenantID.String()))
}

// newSnapshot returns a valid PurchaseSnapshot keyed by a fresh
// PurchaseID â€” tests use this to seed leads through the subscriber-
// shaped factory path.
func newSnapshot(t *testing.T, purchaseID, platformLeadID, buyer string) crmlead.PurchaseSnapshot {
	t.Helper()
	return crmlead.PurchaseSnapshot{
		PurchaseID:              purchaseID,
		PlatformLeadID:          platformLeadID,
		PurchasedByMembershipID: buyer,
		ContactName:             "Test Pharma Store",
		MobileE164:              "+919812345678",
		Email:                   "owner@example.com",
		PinCode:                 "411001",
		City:                    "Pune",
		District:                "Pune",
		State:                   "Maharashtra",
		Street:                  "MG Road 12",
		HasDrugLicence:          true,
		HasGst:                  true,
		GstNumber:               "27ABCDE1234F1Z5",
		HasPan:                  true,
		PanNumber:               "ABCDE1234F",
		BusinessType:            "PCD",
		MedicineSystem:          "Allopathic",
		ProductRanges:           []string{"Antibiotics", "Cardiac"},
		DosageForms:             []string{"Tablet"},
		OrderValue:              "Upto25000",
		BuyTimeline:             "WithinWeek",
	}
}

func TestCrmLeadRepository_Add_PersistsAndEmitsOutbox(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)

	leadID := crmlead.ID(ids.NewV7().String())
	purchaseID := ids.NewV7().String()
	snap := newSnapshot(t, purchaseID, ids.NewV7().String(), ids.NewV7().String())
	l, err := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()), snap, time.Now().UTC())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	if err := repo.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := repo.GetByID(ctx, leadID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Stage() != crmlead.StageNew {
		t.Fatalf("stage: %v", got.Stage())
	}
	if got.SourcePurchaseID() != purchaseID {
		t.Fatalf("purchase id round-trip: got %q want %q", got.SourcePurchaseID(), purchaseID)
	}
	if got.Profile().Extra.GstNumber != "27ABCDE1234F1Z5" {
		t.Fatalf("extra round-trip: %q", got.Profile().Extra.GstNumber)
	}

	// Outbox row written with topic crm.lead_created.v1. The outbox is
	// RLS+FORCE â€” assertion goes through the typed messagingtest helper
	// (Wave 7 + ADR 0047: no raw SQL in tests; helper internalises the
	// platform-GUC bypass once).
	topics := messagingtest.OutboxTopicsForTenant(t, pool, messagingtest.SchemaCRM, tenantID.String())
	if len(topics) != 1 || topics[0] != "crm.lead_created.v1" {
		t.Fatalf("outbox topics: %v", topics)
	}
}

func TestCrmLeadRepository_GetBySourcePurchaseID_FoundAndNotFound(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)
	leadID := crmlead.ID(ids.NewV7().String())
	purchase := ids.NewV7().String()
	snap := newSnapshot(t, purchase, ids.NewV7().String(), ids.NewV7().String())
	l, err := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()), snap, time.Now().UTC())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if err := repo.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Hit
	got, err := repo.GetBySourcePurchaseID(ctx, purchase)
	if err != nil {
		t.Fatalf("GetBySourcePurchaseID: %v", err)
	}
	if got.ID() != leadID {
		t.Fatalf("id mismatch: got %s want %s", got.ID(), leadID)
	}
	// Miss
	_, err = repo.GetBySourcePurchaseID(ctx, ids.NewV7().String())
	if !errors.Is(err, crmlead.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCrmLeadRepository_UpdateByID_StateMachineRoundTrip(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)
	leadID := crmlead.ID(ids.NewV7().String())
	snap := newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String())
	l, _ := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()), snap, time.Now().UTC())
	if err := repo.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}

	actor := ids.NewV7().String()
	if err := repo.UpdateByID(ctx, leadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.ChangeStage(crmlead.StageContacted, actor, "first call", time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateByID stage: %v", err)
	}
	got, err := repo.GetByID(ctx, leadID)
	if err != nil {
		t.Fatalf("GetByID after stage: %v", err)
	}
	if got.Stage() != crmlead.StageContacted {
		t.Fatalf("stage round-trip: %v", got.Stage())
	}

	if err := repo.UpdateByID(ctx, leadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.Convert(actor, time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateByID convert: %v", err)
	}
	got2, _ := repo.GetByID(ctx, leadID)
	if got2.Stage() != crmlead.StageConverted {
		t.Fatalf("convert: %v", got2.Stage())
	}
	if got2.ConvertedByMembershipID() != actor {
		t.Fatalf("convert actor: %q", got2.ConvertedByMembershipID())
	}
}

func TestCrmLeadRepository_ListPage_FilterByStage(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewCrmLeadRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)

	// Seed two leads, advance one to Contacted.
	first := crmlead.ID(ids.NewV7().String())
	l1, _ := crmlead.NewFromPurchaseSnapshot(first, tenant.ID(tenantID.String()),
		newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String()), time.Now().UTC())
	if err := repo.Add(ctx, l1); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	second := crmlead.ID(ids.NewV7().String())
	l2, _ := crmlead.NewFromPurchaseSnapshot(second, tenant.ID(tenantID.String()),
		newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String()), time.Now().UTC())
	if err := repo.Add(ctx, l2); err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	actor := ids.NewV7().String()
	if err := repo.UpdateByID(ctx, second, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.ChangeStage(crmlead.StageContacted, actor, "", time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	// Filter by stage=contacted should return ONLY the second lead.
	page, err := repo.ListPage(ctx, crmlead.ListFilter{Stage: crmlead.StageContacted}, pagination.Cursor{}, 50)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID() != second {
		t.Fatalf("filter stage=contacted returned wrong set: %+v", page.Items)
	}

	// No filter â†’ both leads.
	full, err := repo.ListPage(ctx, crmlead.ListFilter{}, pagination.Cursor{}, 50)
	if err != nil {
		t.Fatalf("ListPage unfiltered: %v", err)
	}
	if len(full.Items) != 2 {
		t.Fatalf("unfiltered count: %d", len(full.Items))
	}
}

func TestCrmLeadRepository_Assign_AndCallLog_RoundTrip(t *testing.T) {
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)
	leads := adapters.NewCrmLeadRepository(pool, tx)
	calls := adapters.NewCallLogRepository(pool, tx)

	tenantID := ids.NewV7()
	ctx := withTenantCtx(t.Context(), tenantID)
	leadID := crmlead.ID(ids.NewV7().String())
	l, _ := crmlead.NewFromPurchaseSnapshot(leadID, tenant.ID(tenantID.String()),
		newSnapshot(t, ids.NewV7().String(), ids.NewV7().String(), ids.NewV7().String()), time.Now().UTC())
	if err := leads.Add(ctx, l); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Assign via UpdateByID â€” handler-side path uses a UoW to combine
	// the lead update + history insert; this test exercises only the
	// repo-side persistence.
	assignee := ids.NewV7().String()
	manager := ids.NewV7().String()
	if err := leads.UpdateByID(ctx, leadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.Assign(assignee, manager, "first routing", time.Now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	got, _ := leads.GetByID(ctx, leadID)
	if got.AssigneeMembershipID() != assignee {
		t.Fatalf("assignee: %q", got.AssigneeMembershipID())
	}

	// Append a call log.
	callID := calllog.ID(ids.NewV7().String())
	cl, err := calllog.New(callID, tenant.ID(tenantID.String()), leadID, calllog.OutcomeInterested, "warm", assignee, time.Now().UTC())
	if err != nil {
		t.Fatalf("calllog.New: %v", err)
	}
	if err := calls.Add(ctx, cl); err != nil {
		t.Fatalf("calls.Add: %v", err)
	}
	rows, err := calls.ListByLead(ctx, leadID)
	if err != nil {
		t.Fatalf("ListByLead: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome() != calllog.OutcomeInterested {
		t.Fatalf("calls row: %+v", rows)
	}
}
