package pg

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// QueryCounter is a pgx.QueryTracer that increments an atomic counter
// on every TraceQueryStart. Integration tests wire it into the pool
// config to assert per-request query budgets — the canonical Go-canon
// N+1 detection vector. Production code never touches it.
//
// Usage in an integration test:
//
//	tracer := &pg.QueryCounter{}
//	cfg.ConnConfig.Tracer = tracer
//	// ... exercise the handler ...
//	require.LessOrEqual(t, tracer.Count(), 3,
//	    "handler issued more than 3 queries (suspected N+1)")
//
// Note: when stacked with otelpgx.NewTracer, only one can occupy
// ConnConfig.Tracer — for production we install otelpgx; for
// integration tests targeting query-count assertions, install
// QueryCounter (or compose via a multi-tracer adapter if you need
// both).
//
// Per pgx README §"Tracing and Logging" + Brandur "What I learned
// running Postgres at scale" — runtime query-count assertion is the
// canonical N+1 detection pattern in Go (Rails `bullet` has no static
// Go equivalent; the pgx tracer hook is what every Go shop uses).
type QueryCounter struct {
	n atomic.Int64
}

// TraceQueryStart implements pgx.QueryTracer. Increments the counter
// and returns ctx unchanged.
func (c *QueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer. No-op.
func (c *QueryCounter) TraceQueryEnd(_ context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {}

// Count returns the number of queries seen since construction or the
// last Reset.
func (c *QueryCounter) Count() int64 { return c.n.Load() }

// Reset zeroes the counter — call between phases of a test to assert
// per-phase budgets.
func (c *QueryCounter) Reset() { c.n.Store(0) }
