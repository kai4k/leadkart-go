//go:build integration

package messaging_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/platform/audit"
	"github.com/leadkart/leadkart-go/internal/platform/messaging"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runRouter starts the router in a goroutine + returns a stop fn. The
// stop fn cancels ctx + waits for Run to return.
func runRouter(t *testing.T, r *messaging.Router) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("router run: %v", err)
		}
	}()
	select {
	case <-r.Running():
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("router did not enter running state within 2s")
	}
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("router did not stop within 5s")
		}
	}
}

func TestRouter_FullStack_TenantContextAndAuditAndIdempotency(t *testing.T) {
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)
	auditW := audit.NewWriter(pool, silentLog())

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentLog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	r, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
		CloseTimeout:     2 * time.Second,
		Retry: messaging.RetryConfig{
			MaxRetries:      1,
			InitialInterval: 10 * time.Millisecond,
			MaxInterval:     50 * time.Millisecond,
			Multiplier:      2.0,
		},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	tenantID := uuid.New()
	type observed struct {
		mu        sync.Mutex
		calls     int
		tenantSet tenancy.ID
	}
	o := &observed{}

	r.AddSubscriber("test.handler", "test.topic",
		func(ctx context.Context, _ string, _ *message.Message) error {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.calls++
			id, _ := tenancy.FromContext(ctx)
			o.tenantSet = id
			return nil
		})

	stop := runRouter(t, r)
	defer stop()

	msgID := uuid.NewString()
	makeMsg := func() *message.Message {
		msg := message.NewMessage(msgID, []byte(`{"hello":"world"}`))
		msg.Metadata.Set(messaging.HeaderTenantID, tenantID.String())
		msg.Metadata.Set(messaging.HeaderEventType, "test.event.v1")
		msg.Metadata.Set(messaging.HeaderOccurredAt, time.Now().UTC().Format(time.RFC3339Nano))
		return msg
	}

	// First publish: handler should run + record dedup row.
	if err := pubsub.Publish("test.topic", makeMsg()); err != nil {
		t.Fatalf("publish 1: %v", err)
	}

	// Wait for first handler completion + dedup record.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		_ = pool.QueryRow(t.Context(), `
			SELECT count(*) FROM identity.processed_messages
			WHERE  message_id = $1 AND handler_name = 'test.handler'
		`, msgID).Scan(&n)
		if n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Sequential replays: with dedup row already committed, subsequent
	// publishes short-circuit on the IdempotentReceiver CHECK.
	for i := range 2 {
		if err := pubsub.Publish("test.topic", makeMsg()); err != nil {
			t.Fatalf("publish replay %d: %v", i, err)
		}
	}
	time.Sleep(300 * time.Millisecond) // let the no-op replays settle

	o.mu.Lock()
	if o.calls != 1 {
		t.Fatalf("handler call count: got %d want 1 (sequential replays dedup)", o.calls)
	}
	if o.tenantSet.String() != tenantID.String() {
		t.Fatalf("tenant ctx: got %q want %q", o.tenantSet, tenantID)
	}
	o.mu.Unlock()

	// Audit row written for the one processed message.
	var auditCount int
	err = pool.QueryRow(t.Context(), `
		SELECT count(*) FROM buildingblocks.audit_log_entry WHERE action = 'test.event.v1'
	`).Scan(&auditCount)
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit rows: got %d want 1", auditCount)
	}

	// Inbox row for the (message, handler) pair.
	var inboxCount int
	err = pool.QueryRow(t.Context(), `
		SELECT count(*) FROM identity.processed_messages
		WHERE  message_id = $1 AND handler_name = 'test.handler'
	`, msgID).Scan(&inboxCount)
	if err != nil {
		t.Fatalf("inbox count: %v", err)
	}
	if inboxCount != 1 {
		t.Fatalf("inbox rows: got %d want 1", inboxCount)
	}
}

func TestRouter_HandlerError_DoesNotRecordInbox_AuditMarksFailure(t *testing.T) {
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)
	auditW := audit.NewWriter(pool, silentLog())

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentLog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	r, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
		CloseTimeout:     2 * time.Second,
		// Tight retry so the test bounds at <1s. Production
		// uses DefaultRetry (~6s exhaustion).
		Retry: messaging.RetryConfig{
			MaxRetries:      1,
			InitialInterval: 10 * time.Millisecond,
			MaxInterval:     50 * time.Millisecond,
			Multiplier:      2.0,
		},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	calls := &atomic.Int32{}
	r.AddSubscriber("test.fail", "test.fail.topic",
		func(ctx context.Context, _ string, _ *message.Message) error {
			calls.Add(1)
			return errors.New("permanent")
		})

	stop := runRouter(t, r)
	defer stop()

	msgID := uuid.NewString()
	msg := message.NewMessage(msgID, []byte(`{}`))
	msg.Metadata.Set(messaging.HeaderEventType, "test.failure.v1")
	if err := pubsub.Publish("test.fail.topic", msg); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for retries to exhaust (1 retry @ 10ms-50ms ≈ <100ms total)
	// + the audit row to be written. 1.5s is a comfortable bound.
	time.Sleep(1500 * time.Millisecond)
	if calls.Load() < 2 {
		t.Fatalf("handler call count: got %d want ≥2 (initial + ≥1 retry)", calls.Load())
	}

	// No inbox row — handler errored.
	var inboxCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM identity.processed_messages WHERE message_id = $1
	`, msgID).Scan(&inboxCount); err != nil {
		t.Fatalf("inbox count: %v", err)
	}
	if inboxCount != 0 {
		t.Fatalf("inbox row recorded despite handler error: got %d want 0", inboxCount)
	}

	// At least one audit row — Succeeded=false.
	var auditCount, failedCount int
	err = pool.QueryRow(t.Context(), `
		SELECT count(*) FROM buildingblocks.audit_log_entry WHERE action = 'test.failure.v1'
	`).Scan(&auditCount)
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	err = pool.QueryRow(t.Context(), `
		SELECT count(*) FROM buildingblocks.audit_log_entry
		WHERE  action = 'test.failure.v1' AND succeeded = false
	`).Scan(&failedCount)
	if err != nil {
		t.Fatalf("failed count: %v", err)
	}
	if auditCount < 1 {
		t.Fatalf("audit rows: got %d want ≥1", auditCount)
	}
	if failedCount < 1 {
		t.Fatalf("failed audit rows: got %d want ≥1", failedCount)
	}
}
