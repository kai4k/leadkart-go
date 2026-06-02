package messaging

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	wmw "github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// DeadLetterWriter persists poisoned messages (salvaged to
// [DeadLetterTopic] by the PoisonQueue middleware) into
// common.dead_letter for inspection and replay.
//
// Mirrors [audit.Writer]: raw pgx against a common table (no
// common sqlc target yet), and best-effort — a failed DLQ write
// is logged and acked, never retried. Retrying a DLQ write would risk a
// loop (the DLQ has no DLQ), and losing a single poison record must not
// wedge the consumer.
type DeadLetterWriter struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	now  func() time.Time
}

// NewDeadLetterWriter wires the persister. now is injected for testable
// timestamps; pass time.Now in production.
func NewDeadLetterWriter(pool *pgxpool.Pool, log *slog.Logger, now func() time.Time) *DeadLetterWriter {
	if pool == nil || log == nil || now == nil {
		panic("messaging: NewDeadLetterWriter requires pool, log, now")
	}
	return &DeadLetterWriter{pool: pool, log: log, now: now}
}

// persist writes one dead_letter row from a poisoned message. The
// PoisonQueue middleware stamps the poison metadata keys
// (wmw.ReasonForPoisonedKey / PoisonedTopicKey / PoisonedHandlerKey).
// Always returns nil — best-effort, ack-on-write-failure (see type doc).
func (w *DeadLetterWriter) persist(ctx context.Context, msg *message.Message) error {
	metaJSON, err := json.Marshal(map[string]string(msg.Metadata))
	if err != nil {
		// Metadata is a string map; marshal cannot realistically fail.
		// If it does, fall back to an empty object so the row still lands.
		metaJSON = []byte("{}")
	}

	_, err = w.pool.Exec(ctx, `
		INSERT INTO common.dead_letter
			(id, topic, handler_name, message_id, reason, payload, metadata, dead_lettered_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`,
		ids.NewV7(),
		msg.Metadata.Get(wmw.PoisonedTopicKey),
		msg.Metadata.Get(wmw.PoisonedHandlerKey),
		msg.UUID,
		msg.Metadata.Get(wmw.ReasonForPoisonedKey),
		msg.Payload,
		metaJSON,
		w.now().UTC(),
	)
	if err != nil {
		// Best-effort: log + ack. A poison record is forensic, not
		// load-bearing; never wedge the consumer on it.
		w.log.ErrorContext(ctx, "dead-letter persist failed",
			"message_id", msg.UUID,
			"poisoned_handler", msg.Metadata.Get(wmw.PoisonedHandlerKey),
			"err", err)
	}
	return nil
}
