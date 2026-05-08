package pg

import (
	"context"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse config: %w", err)
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithIncludeQueryParameters(),
	)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: new pool: %w", err)
	}
	if err := otelpgx.RecordStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: otelpgx stats: %w", err)
	}
	return pool, nil
}
