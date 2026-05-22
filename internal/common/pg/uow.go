package pg

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// UnitOfWork is the consumer-facing transaction primitive. App-layer
// command handlers that need a multi-aggregate single-tx write depend
// on this interface; concrete implementation is [*Transactor].
//
// Per ADR 0047 boundary discipline: app/ may NOT import pgx, pgxpool,
// or the sqlc-generated db package. UnitOfWork is the SOLE
// platform/pg surface a handler is allowed to depend on, and the
// closure runs against `context.Context` only — the underlying pgx.Tx
// is stashed in ctx via [TxFromContext] for adapter consumption,
// invisible to the handler.
//
// Repositories inside `internal/identity/adapters/` automatically
// participate in the surrounding tx: their Add / UpdateByID methods
// check [TxFromContext] and either join the existing tx or open
// their own when the ctx carries none.
type UnitOfWork interface {
	WithinTx(ctx context.Context, scope TxScope, fn func(ctx context.Context) error) error
}

// Compile-time: *Transactor satisfies UnitOfWork via [Transactor.WithinTxCtx]
// (wrapped here as a thin trampoline since Transactor's primary
// WithinTx still exposes pgx.Tx to in-adapter callers).
var _ UnitOfWork = (*Transactor)(nil)

// WithinTx implements [UnitOfWork] by delegating to [Transactor.WithinTx]
// and stashing the underlying pgx.Tx in the closure's ctx via
// [contextWithTx]. The fn receives no pgx.Tx — adapters that need
// the tx pull it out via [TxFromContext].
func (t *Transactor) WithinTx(
	ctx context.Context,
	scope TxScope,
	fn func(ctx context.Context) error,
) error {
	return t.WithinTxPgx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error {
		return fn(contextWithTx(ctx, tx))
	})
}

// txCtxKey is the unexported key under which the active pgx.Tx is
// stashed in context. Unexported = unique-by-type, no collision with
// other packages' context values per Go convention.
type txCtxKey struct{}

// contextWithTx returns a derived ctx carrying the active pgx.Tx.
// Adapter code calls [TxFromContext] to retrieve it.
func contextWithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txCtxKey{}, tx)
}

// TxFromContext returns the active pgx.Tx stashed by [Transactor.WithinTx]
// (the UnitOfWork-shaped variant). Returns (nil, false) when called
// outside a uow.WithinTx closure — adapters use that signal to know
// they need to open their own transaction.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txCtxKey{}).(pgx.Tx)
	return tx, ok
}
