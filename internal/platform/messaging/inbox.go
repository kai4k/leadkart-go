package messaging

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// IdempotentReceiver wraps a Watermill subscriber handler with
// per-(message_id, handler_name) dedup against
// identity.processed_messages.
//
// Per messaging.md "Idempotency — inbox-side required" Layer 2: at-
// least-once delivery is reality; every handler MUST be idempotent
// either via (a) business-key upsert or (b) inbox dedup. This is (b).
//
// The composite (message_id, handler_name) PK means the SAME message
// may be processed by multiple subscribers (one row per pair); each
// (message, handler) combination runs at-most-once over its lifetime.
//
// Order of operations per call (run-then-INSERT-on-conflict canon):
//
//  1. CHECK if (message_id, handler_name) row exists.
//  2. If exists → already processed; no-op return nil.
//  3. Else run the wrapped handler.
//  4. On handler SUCCESS → INSERT (ON CONFLICT DO NOTHING).
//  5. On handler ERROR → don't INSERT; surface the error so the
//     broker re-delivers (handler runs again).
//
// Race tolerance: two concurrent replays of the same (message,
// handler) pair will both pass step 1, both run the handler (which
// MUST be idempotent — that's the whole reason for the dedup), then
// the first to step 4 wins; the second's INSERT silently no-ops on
// the unique constraint. The handler ran twice; that's the
// "at-least-once with idempotent handlers" canonical Watermill shape.
//
// Crash tolerance: process dies AFTER handler success but BEFORE the
// INSERT commits → next replay re-runs the handler (at-least-once).
// Process dies DURING the handler → next replay re-runs (the handler
// must be transactional or compensating internally).
type IdempotentReceiver struct {
	pool *pgxpool.Pool
}

// NewIdempotentReceiver wires the receiver against a pool.
func NewIdempotentReceiver(pool *pgxpool.Pool) *IdempotentReceiver {
	return &IdempotentReceiver{pool: pool}
}

// HandlerFunc is the underlying handler shape — same as Watermill's
// NoPublishHandlerFunc but redeclared so this package has zero
// import-time dependency on watermill (the wiring point in router.go
// adapts).
type HandlerFunc func(ctx context.Context, messageID string) error

// Wrap returns a handler with idempotency enforcement.
//
// handlerName is the persistent identifier of the subscriber handler.
// Convention: dotted path from package + function, e.g.
// "identity.subscribers.RevokeFamiliesOnPasswordChange". Stable
// across deploys — changing this string makes every previously-
// processed message "fresh" and re-runs them all on the next replay.
func (r *IdempotentReceiver) Wrap(handlerName string, fn HandlerFunc) HandlerFunc {
	if handlerName == "" {
		panic("messaging: IdempotentReceiver.Wrap requires non-empty handlerName")
	}
	return func(ctx context.Context, messageID string) error {
		// 1+2. Already processed?
		processed, err := r.alreadyProcessed(ctx, messageID, handlerName)
		if err != nil {
			return fmt.Errorf("inbox: lookup %s/%s: %w", handlerName, messageID, err)
		}
		if processed {
			return nil
		}

		// 3. Run the wrapped handler.
		if herr := fn(ctx, messageID); herr != nil {
			return herr // surface to broker for re-delivery
		}

		// 4. On success, record the (message, handler) pair so future
		//    replays short-circuit. ON CONFLICT DO NOTHING tolerates
		//    the race between two concurrent replays both passing the
		//    initial check.
		_, ierr := r.pool.Exec(ctx, `
			INSERT INTO identity.processed_messages (message_id, handler_name)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, messageID, handlerName)
		if ierr != nil {
			// We've ALREADY run the handler successfully. A failed
			// dedup-row insert means the next replay will run it
			// AGAIN (at-least-once). Acceptable per the contract;
			// surface as an error so observability catches it.
			return fmt.Errorf("inbox: record %s/%s: %w", handlerName, messageID, ierr)
		}
		return nil
	}
}

// alreadyProcessed returns true iff the (message, handler) row exists.
func (r *IdempotentReceiver) alreadyProcessed(ctx context.Context, messageID, handlerName string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM identity.processed_messages
			WHERE  message_id = $1 AND handler_name = $2
		)
	`, messageID, handlerName).Scan(&exists)
	if err != nil {
		// Distinguish "no rows" from real query errors. QueryRow.Scan
		// returns the row's first column, never sql.ErrNoRows for
		// SELECT EXISTS, so any error here is a transport failure.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		return false, err
	}
	return exists, nil
}
