//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container; pgxpool internal conn timeouts + package-level
//   `task ci:test:int -timeout=15m` already bound execution.
//
// arch-test:parallel-safe — every Test* uses a unique (handler_name,
//   message_id) tuple as the inbox-dedup key, so parallel runs cannot
//   collide on the dedup row.

package messaging_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
)

// Shared bootstrap (inboxFixture / TestMain) lives in
// fixture_integration_test.go per the Brandur / TDL canon.

func TestIdempotentReceiver_FirstCall_RunsHandlerAndRecords(t *testing.T) {
	t.Parallel()
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	calls := &atomic.Int32{}
	wrapped := receiver.Wrap("test.handler", func(ctx context.Context, mid string) error {
		calls.Add(1)
		return nil
	})

	if err := wrapped(t.Context(), "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls: got %d want 1", calls.Load())
	}

	// Verify row exists.
	n := messagingtest.InboxCountForMessage(t, pool, "11111111-1111-1111-1111-111111111111", "test.handler")
	if n != 1 {
		t.Fatalf("processed_messages row count: got %d want 1", n)
	}
}

func TestIdempotentReceiver_Replay_SkipsHandler(t *testing.T) {
	t.Parallel()
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	calls := &atomic.Int32{}
	wrapped := receiver.Wrap("test.handler", func(ctx context.Context, mid string) error {
		calls.Add(1)
		return nil
	})

	mid := "22222222-2222-2222-2222-222222222222"
	for i := range 5 {
		if err := wrapped(t.Context(), mid); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls after 5 replays: got %d want 1", calls.Load())
	}
}

func TestIdempotentReceiver_HandlerError_DoesNotRecord_NextCallRunsAgain(t *testing.T) {
	t.Parallel()
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	mid := "33333333-3333-3333-3333-333333333333"
	calls := &atomic.Int32{}
	flaky := receiver.Wrap("test.flaky", func(ctx context.Context, _ string) error {
		n := calls.Add(1)
		if n == 1 {
			return errors.New("transient")
		}
		return nil
	})

	// 1. Handler errors → no dedup row recorded.
	err := flaky(t.Context(), mid)
	if err == nil || err.Error() != "transient" {
		t.Fatalf("first call expected transient err, got %v", err)
	}
	n := messagingtest.InboxCountForMessage(t, pool, mid, "test.flaky")
	if n != 0 {
		t.Fatalf("dedup row recorded after handler error: got %d want 0", n)
	}

	// 2. Retry succeeds — handler runs again, dedup row recorded.
	if err := flaky(t.Context(), mid); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls after retry: got %d want 2", calls.Load())
	}

	// 3. Third call replays — handler does NOT run.
	if err := flaky(t.Context(), mid); err != nil {
		t.Fatalf("third: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("third call ran handler again: got %d want 2", calls.Load())
	}
}

func TestIdempotentReceiver_ScopedByHandlerName(t *testing.T) {
	t.Parallel()
	// Same message_id processed by two distinct handlers — both run.
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	a, b := atomic.Int32{}, atomic.Int32{}
	hA := receiver.Wrap("test.handlerA", func(context.Context, string) error {
		a.Add(1)
		return nil
	})
	hB := receiver.Wrap("test.handlerB", func(context.Context, string) error {
		b.Add(1)
		return nil
	})

	mid := "44444444-4444-4444-4444-444444444444"
	for _, h := range []messaging.HandlerFunc{hA, hB, hA, hB} {
		if err := h(t.Context(), mid); err != nil {
			t.Fatalf("handler: %v", err)
		}
	}
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("counts a=%d b=%d, want 1/1", a.Load(), b.Load())
	}
}

func TestIdempotentReceiver_Wrap_PanicsOnEmptyName(t *testing.T) {
	t.Parallel()
	receiver := messaging.NewIdempotentReceiver(nil) // pool not used in this path
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty handlerName")
		}
	}()
	_ = receiver.Wrap("", func(context.Context, string) error { return nil }) // arch-test:ignore-err — asserts panic; return value unreachable
}
