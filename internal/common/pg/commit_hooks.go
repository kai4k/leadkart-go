package pg

import "context"

// commitHooks collects the functions a transaction runs AFTER it committed.
type commitHooks struct{ fns []func(ctx context.Context) }

type commitHooksKey struct{}

func withCommitHooks(ctx context.Context, h *commitHooks) context.Context {
	return context.WithValue(ctx, commitHooksKey{}, h)
}

// run calls the hooks in registration order. A hook is best-effort follow-up work
// (evicting a cache entry): the transaction is already committed, so a hook has no
// error to return and one that panics must not take the caller's request with it.
func (h *commitHooks) run(ctx context.Context) {
	for _, fn := range h.fns {
		func() {
			defer func() { _ = recover() }()
			fn(ctx)
		}()
	}
}

// OnCommit registers fn to run once the surrounding transaction (the unit of work,
// or the adapter's own transaction when there is none) has COMMITTED; if it rolls
// back, fn never runs. Outside any transaction there is nothing to wait for and fn
// runs immediately. The context fn receives is the transaction's own, so it is
// cancelled with the request: callers that must outlive it derive their own.
//
// Its one use today is evicting the cached security stamp of a person whose stamp
// rotated: evicting before the commit would let a concurrent read refill the cache
// with the old stamp.
func OnCommit(ctx context.Context, fn func(ctx context.Context)) {
	if h, ok := ctx.Value(commitHooksKey{}).(*commitHooks); ok {
		h.fns = append(h.fns, fn)
		return
	}
	fn(ctx)
}
