package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/common/jobs"
)

type fakeReplayer struct {
	n      int
	err    error
	gotNow time.Time
	gotMax int
}

func (f *fakeReplayer) ReplayDue(_ context.Context, now time.Time, limit int) (int, error) {
	f.gotNow, f.gotMax = now, limit
	return f.n, f.err
}

func TestDeadLetterReplayWorker_ReplaysABatchAtTheClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	r := &fakeReplayer{n: 2}
	w := jobs.NewDeadLetterReplayWorker(r, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return fixed })

	if err := w.Work(t.Context(), &river.Job[jobs.DeadLetterReplayJob]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if !r.gotNow.Equal(fixed) || r.gotMax <= 0 {
		t.Fatalf("replayed at %v with batch %d; want the injected clock and a positive batch", r.gotNow, r.gotMax)
	}
	if w.Timeout(&river.Job[jobs.DeadLetterReplayJob]{}) <= 0 {
		t.Fatal("a worker must declare its own timeout")
	}
}

func TestDeadLetterReplayWorker_ReportsAFailureSoRiverRetries(t *testing.T) {
	t.Parallel()
	boom := errors.New("db down")
	w := jobs.NewDeadLetterReplayWorker(&fakeReplayer{err: boom}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := w.Work(t.Context(), &river.Job[jobs.DeadLetterReplayJob]{}); !errors.Is(err, boom) {
		t.Fatalf("Work error = %v, want the replay error", err)
	}
}

// Review G2c N7: the job is on a schedule. A job nobody schedules never runs, and every
// timed-out dead letter then waits for a person.
func TestDeadLetterReplay_IsScheduledEveryMinuteAndNotAtStart(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if got := jobs.DeadLetterReplaySchedule().Next(at); !got.Equal(at.Add(time.Minute)) {
		t.Fatalf("next run %v, want one minute after %v", got, at)
	}
	if jobs.DeadLetterReplayPeriodicOpts().RunOnStart {
		t.Fatal("a worker restart must not add a run")
	}
	if jobs.DeadLetterReplayPeriodicJob() == nil {
		t.Fatal("no periodic job")
	}
	if kind := (jobs.DeadLetterReplayJob{}).Kind(); kind != "common.dead_letter_replay" {
		t.Fatalf("job kind %q changed; river kinds are stable", kind)
	}
}
