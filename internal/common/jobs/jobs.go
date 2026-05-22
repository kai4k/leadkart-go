// Package jobs wires the River background-job worker pool that
// cmd/worker hosts. Per ADR 0010: river is the chosen queue (Brandur
// Leach, same author as the sqlc driver canon we already use), backed
// by the same Postgres pool the application uses.
//
// One River client per process; each binary that needs jobs (cmd/worker
// for now; future cmd/api admin chores if we ever need them) wires its
// own. The river_* schema lives in Postgres alongside the application
// schema — production migrations land via goose; dev auto-migrates on
// boot via [Migrate].
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

// Migrate runs River's own up-migrations against pool. Idempotent —
// no-op if the schema is already at the latest revision.
//
// Called by cmd/worker once at boot in the v0.2 deploy shape (single
// worker replica). v0.3 splits this into a separate cmd/migrate
// invocation alongside the goose application migrations so the
// runtime worker no longer needs migration permissions on the DB.
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
