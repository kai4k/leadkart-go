package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wmsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/components/forwarder"
	"github.com/ThreeDotsLabs/watermill/message"
	wmw "github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// ReplayTargetKey is the message metadata that names the ONE handler a replayed
// dead letter is for. The topic's other handlers ignore the message: they already
// processed it (the inbox would drop it) or dead-lettered it for their own reasons.
const ReplayTargetKey = "replay_for_handler"

// MaxDeadLetterReplays bounds how many times one (message, handler) pair is replayed
// after timing out. A message that times out every time stays in common.dead_letter,
// kept, for a person to look at.
const MaxDeadLetterReplays = 3

// replayBackoff is how long after a dead-lettering the n-th replay of that pair
// waits (the first replay after one minute, the last after half an hour). Its length
// is [MaxDeadLetterReplays].
var replayBackoff = [MaxDeadLetterReplays]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// DeadLetterReplayer delivers timed-out dead letters again.
//
// A handler attempt that hits its timeout is retried at most once on the delivery
// lane (a stuck handler holds the lane for every company), then dead-lettered. This
// puts such a message back on the outbox, targeted at the handler that timed out, after
// a backoff and at most [MaxDeadLetterReplays] times. The dead-letter row is never
// changed or removed; each replay is one row in common.dead_letter_replay. The inbox
// keeps a replay that races a late success from running the handler twice.
//
// Replay is opt-in: only the handlers named at construction (those their module marked
// with [ReplaySafe]) are replayed. Every other handler's dead letters stay for a person.
type DeadLetterReplayer struct {
	pool       *pgxpool.Pool
	log        *slog.Logger
	replaySafe []string
}

// NewDeadLetterReplayer wires the replayer. replaySafe names the handlers whose timed-out
// dead letters may be delivered again ([ReplaySafeHandlerNames]); none means none.
func NewDeadLetterReplayer(pool *pgxpool.Pool, log *slog.Logger, replaySafe []string) *DeadLetterReplayer {
	if pool == nil || log == nil {
		panic("messaging: NewDeadLetterReplayer requires pool, log")
	}
	return &DeadLetterReplayer{pool: pool, log: log, replaySafe: append([]string{}, replaySafe...)}
}

// dueReplayQuery picks timed-out dead letters that have no replay of their own yet,
// whose (message, handler) pair is under the replay bound and whose backoff for its
// next replay has passed at $1. $2 the timeout reason prefix, $3 the backoff in
// seconds (index n = the wait before replay n+1), $4 the batch size, $5 one handler
// name or empty for every handler, $6 the handlers marked replay-safe: any other
// handler's dead letter is never picked.
const dueReplayQuery = `
SELECT d.id, d.topic, d.handler_name, d.message_id, d.payload, d.metadata
FROM   common.dead_letter d
CROSS  JOIN LATERAL (
           SELECT count(*)::int AS n FROM common.dead_letter_replay r
           WHERE  r.message_id = d.message_id AND r.handler_name = d.handler_name
       ) done
WHERE  starts_with(d.reason, $2)
AND    ($5 = '' OR d.handler_name = $5)
AND    d.handler_name = ANY($6::text[])
AND    done.n < cardinality($3::float8[])
AND    NOT EXISTS (SELECT 1 FROM common.dead_letter_replay r WHERE r.dead_letter_id = d.id)
AND    d.dead_lettered_at + make_interval(secs => ($3::float8[])[done.n + 1]) <= $1
ORDER  BY d.dead_lettered_at, d.id
LIMIT  $4`

type dueReplay struct {
	id      uuid.UUID
	topic   string
	handler string
	msgID   string
	payload []byte
	meta    []byte
}

// ReplayDue puts up to limit due timed-out dead letters back on the outbox and
// returns how many. Each is one transaction: the replay row and the outbox row commit
// together, so a crash loses neither and repeats neither.
func (r *DeadLetterReplayer) ReplayDue(ctx context.Context, now time.Time, limit int) (int, error) {
	return r.ReplayDueFor(ctx, now, limit, "")
}

// ReplayDueFor is [DeadLetterReplayer.ReplayDue] limited to the dead letters of one
// handler (empty: every handler).
func (r *DeadLetterReplayer) ReplayDueFor(ctx context.Context, now time.Time, limit int, handlerName string) (int, error) {
	backoff := make([]float64, len(replayBackoff))
	for i, d := range replayBackoff {
		backoff[i] = d.Seconds()
	}
	rows, err := r.pool.Query(ctx, dueReplayQuery, now.UTC(), TimeoutReasonPrefix, backoff, limit, handlerName, r.replaySafe)
	if err != nil {
		return 0, fmt.Errorf("messaging: select due dead letters: %w", err)
	}
	var due []dueReplay
	for rows.Next() {
		var d dueReplay
		if err := rows.Scan(&d.id, &d.topic, &d.handler, &d.msgID, &d.payload, &d.meta); err != nil {
			rows.Close()
			return 0, fmt.Errorf("messaging: scan due dead letter: %w", err)
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("messaging: select due dead letters: %w", err)
	}

	replayed := 0
	for _, d := range due {
		ok, err := r.replayOne(ctx, d, now)
		if err != nil {
			return replayed, err
		}
		if ok {
			replayed++
			r.log.InfoContext(ctx, "dead letter replayed", "message_id", d.msgID, "handler", d.handler, "topic", d.topic)
		}
	}
	return replayed, nil
}

// replayOne records the replay and writes the outbox row in one transaction. It
// reports false when another run already replayed this dead letter.
func (r *DeadLetterReplayer) replayOne(ctx context.Context, d dueReplay, now time.Time) (bool, error) {
	meta := map[string]string{}
	if err := json.Unmarshal(d.meta, &meta); err != nil {
		return false, fmt.Errorf("messaging: dead letter %s metadata: %w", d.msgID, err)
	}
	for _, k := range []string{wmw.ReasonForPoisonedKey, wmw.PoisonedTopicKey, wmw.PoisonedHandlerKey} {
		delete(meta, k)
	}
	meta[ReplayTargetKey] = d.handler
	msg := message.NewMessage(d.msgID, d.payload)
	msg.Metadata = meta

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("messaging: begin replay: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // arch-test:ignore-err — a committed tx makes this a no-op
	tag, err := tx.Exec(ctx, `
		INSERT INTO common.dead_letter_replay (id, dead_letter_id, message_id, handler_name, replayed_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (dead_letter_id) DO NOTHING`,
		ids.NewV7(), d.id, d.msgID, d.handler, now.UTC())
	if err != nil {
		return false, fmt.Errorf("messaging: record replay: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := publishToOutbox(tx, d.topic, msg); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("messaging: commit replay: %w", err)
	}
	return true, nil
}

// publishToOutbox writes one raw message to the outbox inside tx, bound for topic.
func publishToOutbox(tx pgx.Tx, topic string, msg *message.Message) error {
	sqlPub, err := wmsql.NewPublisher(wmsql.TxFromPgx(tx),
		wmsql.PublisherConfig{SchemaAdapter: OutboxQueueSchema()}, watermill.NopLogger{})
	if err != nil {
		return fmt.Errorf("outbox: new sql publisher: %w", err)
	}
	if err := forwarder.NewPublisher(sqlPub, forwarder.PublisherConfig{ForwarderTopic: OutboxForwarderTopic}).Publish(topic, msg); err != nil {
		return fmt.Errorf("outbox: publish replay: %w", err)
	}
	return nil
}

// replayTarget drops a replayed message that is for another handler. Without a
// target (every first delivery) it passes the message through.
func replayTarget(handlerName string) message.HandlerMiddleware {
	return func(h message.HandlerFunc) message.HandlerFunc {
		return func(msg *message.Message) ([]*message.Message, error) {
			if target := msg.Metadata.Get(ReplayTargetKey); target != "" && target != handlerName {
				return nil, nil
			}
			return h(msg)
		}
	}
}
