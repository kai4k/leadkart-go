package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/identity/app/jobs"
)

type fakePurger struct {
	batches []int64 // rows returned per call, in order
	calls   int
	nows    []time.Time
	sizes   []int
	err     error
}

func (f *fakePurger) PurgeExpired(_ context.Context, now time.Time, batch int) (int64, error) {
	f.nows = append(f.nows, now)
	f.sizes = append(f.sizes, batch)
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	if f.calls > len(f.batches) {
		return 0, nil
	}
	return f.batches[f.calls-1], nil
}

func newWorker(p *fakePurger) *jobs.LoginPhantomPurgeWorker {
	fixed := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return jobs.NewLoginPhantomPurgeWorker(p, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return fixed })
}

func TestLoginPhantomPurge_RepeatsUntilABatchComesBackShort(t *testing.T) {
	t.Parallel()
	full := int64(jobs.LoginPhantomPurgeBatchSize)
	p := &fakePurger{batches: []int64{full, full, 17}}
	if err := newWorker(p).Work(t.Context(), &river.Job[jobs.LoginPhantomPurgeJob]{}); err != nil {
		t.Fatal(err)
	}
	if p.calls != 3 {
		t.Fatalf("%d purge calls, want 3 (two full batches, then a short one)", p.calls)
	}
	for i, size := range p.sizes {
		if size != jobs.LoginPhantomPurgeBatchSize {
			t.Fatalf("call %d batch size %d, want the constant %d", i, size, jobs.LoginPhantomPurgeBatchSize)
		}
		if !p.nows[i].Equal(p.nows[0]) {
			t.Fatal("the cutoff moved between batches of one run")
		}
	}
}

func TestLoginPhantomPurge_OneRunIsBounded(t *testing.T) {
	t.Parallel()
	// A backlog that never shrinks: the run still ends (the next tick continues).
	p := &fakePurger{batches: make([]int64, 10_000)}
	for i := range p.batches {
		p.batches[i] = int64(jobs.LoginPhantomPurgeBatchSize)
	}
	if err := newWorker(p).Work(t.Context(), &river.Job[jobs.LoginPhantomPurgeJob]{}); err != nil {
		t.Fatal(err)
	}
	if p.calls != 200 {
		t.Fatalf("%d batches in one run, want the cap of 200", p.calls)
	}
}

func TestLoginPhantomPurge_ReturnsStorageErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	if err := newWorker(&fakePurger{err: boom}).Work(t.Context(), &river.Job[jobs.LoginPhantomPurgeJob]{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the storage error so River retries", err)
	}
}

func TestLoginPhantomPurge_KindAndTimeoutAreDeclared(t *testing.T) {
	t.Parallel()
	if got := (jobs.LoginPhantomPurgeJob{}).Kind(); got != "identity.login_phantom_purge" {
		t.Fatalf("kind = %q", got)
	}
	if d := newWorker(&fakePurger{}).Timeout(&river.Job[jobs.LoginPhantomPurgeJob]{}); d <= 0 {
		t.Fatalf("timeout = %v", d)
	}
}
