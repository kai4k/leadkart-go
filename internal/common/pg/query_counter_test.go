package pg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

func TestQueryCounter_CountsTraceQueryStarts(t *testing.T) {
	t.Parallel()

	c := &pg.QueryCounter{}
	if got := c.Count(); got != 0 {
		t.Fatalf("Count() = %d, want 0 on fresh counter", got)
	}

	for range 5 {
		c.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{})
	}
	if got := c.Count(); got != 5 {
		t.Errorf("Count() = %d, want 5", got)
	}
}

func TestQueryCounter_ResetZeroes(t *testing.T) {
	t.Parallel()

	c := &pg.QueryCounter{}
	c.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{})
	c.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{})
	c.Reset()
	if got := c.Count(); got != 0 {
		t.Errorf("Count() after Reset = %d, want 0", got)
	}
}

func TestQueryCounter_TraceQueryEnd_IsNoOp(t *testing.T) {
	t.Parallel()

	c := &pg.QueryCounter{}
	c.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{})
	c.TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{})
	if got := c.Count(); got != 1 {
		t.Errorf("Count() after End-only = %d, want 1 (Start counts, End is no-op)", got)
	}
}
