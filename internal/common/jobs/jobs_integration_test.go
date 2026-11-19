//go:build integration

// arch-test:raw-sql-justified — probes pg_catalog privileges of the application role; there is no typed helper for catalog introspection.

package jobs_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/jobs"
	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{TruncateSchemas: []string{"common"}}, func(c *pgtest.Container) {
		sharedPG = c
	})
	if err := goleak.Find(pgtest.GoleakOptions()...); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

type pingArgs struct{}

func (pingArgs) Kind() string { return "jobs_integration_ping" }

type pingWorker struct {
	river.WorkerDefaults[pingArgs]
	ran chan struct{}
}

func (w *pingWorker) Work(context.Context, *river.Job[pingArgs]) error {
	w.ran <- struct{}{}
	return nil
}

// The worker runs as leadkart_app, which has no CREATE on public. River's tables
// must therefore be created and granted by the owner (what cmd/migrate does) and
// the application role's client must then work on exactly those grants.
func TestRiverWorkerRunsAsApplicationRoleWithoutCreateOnPublic(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	appPool := sharedPG.Pool()
	ownerPool, err := pgxpool.New(ctx, sharedPG.OwnerDSN())
	if err != nil {
		t.Fatalf("owner pool: %v", err)
	}
	defer ownerPool.Close()

	var canCreate bool
	if err := appPool.QueryRow(ctx, `SELECT has_schema_privilege(current_user, 'public', 'CREATE')`).Scan(&canCreate); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if canCreate {
		t.Fatal("the application role must not have CREATE on public")
	}

	// Before River is migrated the worker's start-up check says so, clearly.
	if err := jobs.RequireSchema(ctx, appPool); err == nil || !strings.Contains(err.Error(), "migrate up") {
		t.Fatalf("RequireSchema before migrating: want an error pointing at `migrate up`, got %v", err)
	}
	// The application role cannot run River's migration (the reason it moved to cmd/migrate).
	if err := jobs.Migrate(ctx, appPool); err == nil {
		t.Fatal("River migration succeeded as the application role: it must not be able to create tables")
	}

	if err := jobs.Migrate(ctx, ownerPool); err != nil {
		t.Fatalf("owner river migrate: %v", err)
	}
	if err := jobs.RequireSchema(ctx, appPool); err != nil {
		t.Fatalf("RequireSchema after migrating: %v", err)
	}

	newClient := func() (*jobs.Client, chan struct{}) {
		ran := make(chan struct{}, 1)
		workers := river.NewWorkers()
		river.AddWorker(workers, &pingWorker{ran: ran})
		c, err := jobs.NewClient(jobs.Config{Pool: appPool, Workers: workers})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		return c, ran
	}

	// Migrated but not yet granted: the application role cannot even enqueue.
	c, _ := newClient()
	if _, err := c.Insert(ctx, pingArgs{}, nil); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("insert before grants: want permission denied, got %v", err)
	}

	if err := jobs.GrantApplicationRole(ctx, ownerPool); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := jobs.GrantApplicationRole(ctx, ownerPool); err != nil {
		t.Fatalf("grant is not idempotent: %v", err)
	}

	c, ran := newClient()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start as application role: %v", err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := c.Stop(stopCtx); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()
	if _, err := c.Insert(ctx, pingArgs{}, nil); err != nil {
		t.Fatalf("insert as application role: %v", err)
	}
	select {
	case <-ran:
	case <-ctx.Done():
		t.Fatal("job never ran as the application role")
	}

	// And only DML: no DDL, no TRUNCATE.
	if _, err := appPool.Exec(ctx, `TRUNCATE public.river_job`); err == nil {
		t.Fatal("application role can TRUNCATE river_job")
	}
}
