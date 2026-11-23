package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// DeadLetterReplayInterval is how often the replay job looks for timed-out dead
// letters whose backoff has passed.
const DeadLetterReplayInterval = time.Minute

// deadLetterReplayBatch bounds one run; the next tick continues.
const deadLetterReplayBatch = 100

// deadLetterReplayTimeout is the River per-job timeout.
const deadLetterReplayTimeout = time.Minute

// DeadLetterReplayJob is the river.JobArgs payload (empty: it works against the
// dead-letter table and the clock).
type DeadLetterReplayJob struct{}

// Kind returns the river-stable job kind. Never rename.
func (DeadLetterReplayJob) Kind() string { return "common.dead_letter_replay" }

// DeadLetterReplayer puts due timed-out dead letters back on the outbox and returns
// how many. Consumer-defined; messaging.DeadLetterReplayer implements it.
type DeadLetterReplayer interface {
	ReplayDue(ctx context.Context, now time.Time, limit int) (int, error)
}

// DeadLetterReplayWorker is the river.Worker for [DeadLetterReplayJob].
//
// river:idempotent - a dead letter is replayed at most once (the replay row is unique
// per dead letter and written in the same transaction as the outbox row), so a retry
// or an overlapping run only finds less to replay; the inbox keeps a replayed event
// from running its handler twice.
type DeadLetterReplayWorker struct {
	river.WorkerDefaults[DeadLetterReplayJob]

	replayer DeadLetterReplayer
	log      *slog.Logger
	now      func() time.Time
}

// NewDeadLetterReplayWorker wires the worker. now is the explicit clock (nil = time.Now).
func NewDeadLetterReplayWorker(replayer DeadLetterReplayer, log *slog.Logger, now func() time.Time) *DeadLetterReplayWorker {
	if replayer == nil {
		panic("jobs: NewDeadLetterReplayWorker replayer required")
	}
	if log == nil {
		panic("jobs: NewDeadLetterReplayWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &DeadLetterReplayWorker{replayer: replayer, log: log, now: now}
}

// Timeout bounds one run.
func (*DeadLetterReplayWorker) Timeout(*river.Job[DeadLetterReplayJob]) time.Duration {
	return deadLetterReplayTimeout
}

// Work replays one batch of due dead letters.
func (w *DeadLetterReplayWorker) Work(ctx context.Context, _ *river.Job[DeadLetterReplayJob]) error {
	n, err := w.replayer.ReplayDue(ctx, w.now(), deadLetterReplayBatch)
	if err != nil {
		return fmt.Errorf("dead letter replay: %w", err)
	}
	if n > 0 {
		w.log.InfoContext(ctx, "dead letter replay complete", "replayed", n)
	}
	return nil
}
