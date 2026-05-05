package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TxScope describes how a transaction should bind tenant context.
type TxScope int

const (
	// TxScopeTenant binds the request's tenant to app.tenant_id GUC and
	// keeps app.is_platform = false. The default for in-tenant flows.
	TxScopeTenant TxScope = iota

	// TxScopePlatform sets app.is_platform = true for the duration of
	// the transaction, bypassing RLS. Reserved for outbox forwarder,
	// cross-tenant maintenance, and bootstrap paths.
	TxScopePlatform
)

// Transactor wraps a pgxpool with the TDL UpdateFn pattern (Sep 2024 +
// Wild Workouts canon): every state-changing flow is a closure executed
// inside a transaction. The transactor owns BeginTx + commit/rollback +
// tenant-GUC binding so domain code never thinks about tx mechanics.
//
// Usage:
//
//	err := tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, q pgx.Tx) error {
//	    // run sqlc queries against q
//	    return nil
//	})
//
// Per ADR 0004: repository.UpdateByID composes WithinTx with load → mutate
// → persist → outbox in a single closure.
type Transactor struct {
	pool *pgxpool.Pool
}

// NewTransactor wraps a pgxpool. Pool ownership stays with the caller —
// the transactor does not Close it.
func NewTransactor(pool *pgxpool.Pool) *Transactor {
	return &Transactor{pool: pool}
}

// WithinTx opens a transaction, binds tenant context per scope, runs fn,
// and commits if fn returns nil (rolls back otherwise). If fn returns a
// non-nil error, the rollback error is intentionally discarded — the
// returned error is the original failure.
//
// Default isolation is pgx default (READ COMMITTED). Repositories that
// need REPEATABLE READ or SERIALIZABLE wrap WithinTx with their own
// retry loop on `40001` serialization failures.
func (t *Transactor) WithinTx(
	ctx context.Context,
	scope TxScope,
	fn func(ctx context.Context, tx pgx.Tx) error,
) error {
	tx, err := t.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("pg: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Rollback errors are best-effort — the original error from fn
			// (or commit) is what the caller cares about.
			_ = tx.Rollback(ctx)
		}
	}()

	if err := bindScope(ctx, tx, scope); err != nil {
		return err
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: commit tx: %w", err)
	}
	committed = true
	return nil
}

func bindScope(ctx context.Context, tx pgx.Tx, scope TxScope) error {
	switch scope {
	case TxScopeTenant:
		return SetTenantOnTx(ctx, tx)
	case TxScopePlatform:
		return SetPlatformOnTx(ctx, tx)
	default:
		return errors.New("pg: unknown TxScope")
	}
}
