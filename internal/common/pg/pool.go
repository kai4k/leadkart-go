package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool bounds — defaults chosen for a single-replica v0.2 deployment
// against managed Postgres (Crunchy / Supabase / RDS). Sized so a small
// cluster (~4 vCPU) can absorb steady traffic without exhausting the
// server-side max_connections cap.
//
// Brandur Leach "Postgres at Scale" + Crunchy Data sizing guide canon
// for sub-1Gbps workloads: pool size = cores * 2 + spindles. Values
// stay conservative; operators override via PoolConfig.
const (
	defaultMaxConns        = 16
	defaultMinConns        = 2
	defaultMaxConnLifetime = 30 * time.Minute
	defaultMaxConnIdleTime = 5 * time.Minute
	defaultHealthCheckTick = 30 * time.Second
)

// PoolConfig configures [NewPool]. Zero values fall back to the
// pkg-level default* constants — every field is optional, but every
// bound is set explicitly so the arch-test gate is satisfied + so the
// pool never inherits a surprise pgx default.
type PoolConfig struct {
	// IncludeQueryParameters captures bound parameter values on every
	// span. OFF by default per OTel semconv §db.statement.parameters
	// ("MUST NOT be captured by default") because parameter values
	// for queries on identity.persons / refresh_token_families
	// include hash inputs, security stamps, and other PII / secret
	// material. Turn ON only in dev when debugging a specific query.
	IncludeQueryParameters bool

	// MaxConns caps concurrent active connections. Zero → defaultMaxConns.
	// Override when the host's max_connections cap is shared across many
	// replicas (k * replicas <= cap).
	MaxConns int32
	// MinConns is the warm-pool floor. Zero → defaultMinConns. Keeps a
	// few connections hot so the first request after idle doesn't pay
	// TCP+TLS setup latency.
	MinConns int32
	// MaxConnLifetime forces a connection to be re-established after
	// this age. Zero → defaultMaxConnLifetime. Caps blast radius of
	// long-lived sessions (TLS rotation, plan-cache bloat).
	MaxConnLifetime time.Duration
	// MaxConnIdleTime evicts idle connections older than this. Zero →
	// defaultMaxConnIdleTime. Aligns the pool with managed-PG idle
	// killers (RDS default 30m, Supabase pgbouncer 60s).
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod sets the background health-check tick. Zero →
	// defaultHealthCheckTick.
	HealthCheckPeriod time.Duration
}

// NewPool parses dsn into a pgxpool.Config + installs the otelpgx
// QueryTracer + StatsTracer so every Postgres query the application
// runs is captured as an OTel span (+ pool-stat metrics). Edges-only
// HTTP tracing misses the time spent in the database — Honeycomb /
// Charity Majors canon ("instrumentation at edges AND middle") and
// audit-checklist.md §12 both require DB spans.
//
// Connection-pool bounds are set explicitly per Brandur "Postgres at
// Scale" + the pgx README §"Connection Pool Configuration" — relying
// on pgx defaults (MaxConns = max(4, runtime.NumCPU())) is a common
// production fire when a host's max_connections cap is shared.
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

	// Apply pool bounds: caller value wins, otherwise the pkg default.
	pcfg.MaxConns = cfg.MaxConns
	if pcfg.MaxConns <= 0 {
		pcfg.MaxConns = defaultMaxConns
	}
	pcfg.MinConns = cfg.MinConns
	if pcfg.MinConns <= 0 {
		pcfg.MinConns = defaultMinConns
	}
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	if pcfg.MaxConnLifetime <= 0 {
		pcfg.MaxConnLifetime = defaultMaxConnLifetime
	}
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	if pcfg.MaxConnIdleTime <= 0 {
		pcfg.MaxConnIdleTime = defaultMaxConnIdleTime
	}
	pcfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	if pcfg.HealthCheckPeriod <= 0 {
		pcfg.HealthCheckPeriod = defaultHealthCheckTick
	}

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
