package pg

import (
	"context"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig configures [NewPool]. Currently only the parameter-
// capture switch lives here; future tuning knobs (max conns, idle
// timeout, statement cache mode) will accumulate as fields rather
// than positional args.
type PoolConfig struct {
	// IncludeQueryParameters captures bound parameter values on every
	// span. OFF by default per OTel semconv §db.statement.parameters
	// ("MUST NOT be captured by default") because parameter values
	// for queries on identity.persons / refresh_token_families
	// include hash inputs, security stamps, and other PII / secret
	// material. Turn ON only in dev when debugging a specific query.
	IncludeQueryParameters bool
}

// NewPool parses dsn into a pgxpool.Config + installs the otelpgx
// QueryTracer + StatsTracer so every Postgres query the application
// runs is captured as an OTel span (+ pool-stat metrics). Edges-only
// HTTP tracing misses the time spent in the database — Honeycomb /
// Charity Majors canon ("instrumentation at edges AND middle") and
// audit-checklist.md §12 both require DB spans.
//
// Wiring lives here so cmd/api + cmd/worker share one canonical pool
// constructor — without that, one binary's spans would be richer
// than the other's depending on whose main.go got updated first.
//
// Tests that don't care about traces continue to call pgxpool.New
// directly; the no-op TracerProvider installed by obs.Setup makes
// the otelpgx hooks zero-cost in dev anyway, but skipping here keeps
// test fixtures small.
func NewPool(ctx context.Context, dsn string, cfg PoolConfig) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse config: %w", err)
	}
	tracerOpts := []otelpgx.Option{}
	if cfg.IncludeQueryParameters {
		tracerOpts = append(tracerOpts, otelpgx.WithIncludeQueryParameters())
	}
	pcfg.ConnConfig.Tracer = otelpgx.NewTracer(tracerOpts...)
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("pg: new pool: %w", err)
	}
	if err := otelpgx.RecordStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: otelpgx stats: %w", err)
	}
	return pool, nil
}
