// Package jobs wires the River background-job worker pool that
// cmd/worker hosts. Per ADR 0010: river is the chosen queue (Brandur
// Leach, same author as the sqlc driver canon we already use), backed
// by the same Postgres pool the application uses.
//
// One River client per process; each binary that needs jobs (cmd/worker
// for now; future cmd/api admin chores if we ever need them) wires its
// own. The river_* tables live in schema public, created by [Migrate] from
// cmd/migrate (as the owner, after goose) and granted to the application role by
// [GrantApplicationRole]; the worker only checks them with [RequireSchema].
//
// Periodic jobs (e.g. AuditLogPurgeJob) are registered on the Config
// before NewClient. River's scheduler runs them in-process using the
// canonical Postgres advisory-lock-based leader election so a multi-
// replica worker deploy still runs each periodic at most once.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// Migrate runs River's own up-migrations against pool. Idempotent: a no-op if
// the schema is already at the latest revision.
//
// It needs CREATE on public (River creates its tables there), which the
// application role does not and must not have. It therefore runs ONLY from
// cmd/migrate as the owner role, right after the goose migrations, followed by
// [GrantApplicationRole]. The worker does not call it at boot; it calls
// [RequireSchema] and fails fast when the River tables are missing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("jobs: rivermigrate.New: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("jobs: river migrate: %w", err)
	}
	return nil
}

// appRole is the application runtime role (docker/postgres/init.sql).
const appRole = "leadkart_app"

// grantSQL gives the application role what the River client needs on the
// public.river_* tables and sequences: DML on tables (the client inserts,
// fetches and completes jobs, elects a leader, tracks queues), USAGE+SELECT on
// sequences. river_migration is River's own ledger, read and written only by
// [Migrate], so the application role gets nothing on it. Never TRUNCATE, never
// DDL. No-op (NOTICE) when the role does not exist.
const grantSQL = `
DO $$
DECLARE
    r record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + appRole + `') THEN
        RAISE NOTICE 'role ` + appRole + ` does not exist: river grants skipped';
        RETURN;
    END IF;
    FOR r IN
        SELECT c.relname, c.relkind
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relname LIKE 'river\_%'
          AND c.relname <> 'river_migration' AND c.relkind IN ('r', 'p', 'S')
    LOOP
        IF r.relkind = 'S' THEN
            EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE public.%I TO ` + appRole + `', r.relname);
        ELSE
            EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.%I TO ` + appRole + `', r.relname);
        END IF;
    END LOOP;
END
$$`

// GrantApplicationRole grants the application role its privileges on the River
// tables. River's tables come from River's own migration ledger, not goose, so
// the goose grants migration cannot cover them on a fresh database (they do not
// exist yet when it runs). cmd/migrate calls it after [Migrate] on every
// `migrate up`: idempotent, and it also covers tables a later River version
// adds, on existing and fresh databases alike. Run as the owner.
func GrantApplicationRole(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, grantSQL); err != nil {
		return fmt.Errorf("jobs: grant river privileges: %w", err)
	}
	return nil
}

// RequireSchema fails with a clear message when River's tables are missing, so
// a worker started before `migrate up` stops at boot instead of failing job by
// job. Needs no privilege beyond catalog reads.
func RequireSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.river_job') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("jobs: check river schema: %w", err)
	}
	if !present {
		return errors.New("jobs: river tables are missing: run `migrate up` as the owner role (it applies River's migrations and grants the application role) before starting the worker")
	}
	return nil
}

// Config bundles the dependencies the [Client] needs.
type Config struct {
	Pool         *pgxpool.Pool
	Workers      *river.Workers
	PeriodicJobs []*river.PeriodicJob
	Logger       *slog.Logger
	// Queues maps queue name → MaxWorkers. Defaults to one default
	// queue with 10 workers if nil — sufficient for the v0.2 single-
	// job (audit purge) load.
	Queues map[string]river.QueueConfig
}

// Client wraps a [*river.Client] for [pgxpool.Pool] transactions.
// The generic param is [pgx.Tx] — the type pgxpool.Pool.BeginTx
// returns — not a separate pgxpool.Tx type.
type Client = river.Client[pgx.Tx]

// NewClient constructs the River client. Caller defers Stop on
// shutdown via the returned client's Stop / StopAndCancel methods.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Pool == nil {
		return nil, errors.New("jobs: Pool required")
	}
	if cfg.Workers == nil {
		return nil, errors.New("jobs: Workers required (call river.NewWorkers + AddWorker before NewClient)")
	}
	queues := cfg.Queues
	if queues == nil {
		queues = map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		}
	}
	rcfg := &river.Config{
		Queues:       queues,
		Workers:      cfg.Workers,
		PeriodicJobs: cfg.PeriodicJobs,
		Logger:       cfg.Logger,
	}
	c, err := river.NewClient(riverpgxv5.New(cfg.Pool), rcfg)
	if err != nil {
		return nil, fmt.Errorf("jobs: river.NewClient: %w", err)
	}
	return c, nil
}
