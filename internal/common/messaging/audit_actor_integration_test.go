//go:build integration

// arch-test:no-timeout-needed — bounded polling deadline below; the router
// fixture owns the container timeout.
//
// arch-test:no-synctest — the audit row is written by a Watermill goroutine
// across the pgx wire protocol, which synctest cannot model.

package messaging_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/audit/audittest"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// auditRouter wires a router with the real audit writer and one subscriber
// whose handler returns handlerErr.
func auditRouter(t *testing.T, pool *pgxpool.Pool, topic string, handlerErr error) *gochannel.GoChannel {
	t.Helper()
	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentLog()))
	t.Cleanup(func() { _ = pubsub.Close() })
	r, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Publisher:        pubsub,
		Logger:           silentLog(),
		IdempotencyInbox: messaging.NewIdempotentReceiver(pool),
		AuditWriter:      audit.NewWriter(pool, silentLog(), time.Now),
		DeadLetters:      messaging.NewDeadLetterWriter(pool, silentLog(), time.Now),
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
	r.AddSubscriber(topic, topic, func(_ context.Context, _ string, _ *message.Message) error { return handlerErr })
	stop := runRouter(t, r)
	t.Cleanup(stop)
	return pubsub
}

// waitAuditRow polls for the audit row of action and returns its user_id,
// succeeded flag and failure_reason.
func waitAuditRow(t *testing.T, pool *pgxpool.Pool, action string) (userID *uuid.UUID, succeeded bool, failure *string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	// arch-test:wait-justified — polling an audit row written by an async subscriber; bounded by deadline
	for time.Now().Before(deadline) {
		if r, ok := audittest.RowByAction(t, pool, action); ok {
			return r.UserID, r.Succeeded, r.FailureReason
		}
		time.Sleep(20 * time.Millisecond) // arch-test:wait-justified — wait-until poll for async audit write
	}
	t.Fatalf("no audit row for %s within 5s", action)
	return nil, false, nil
}

func TestAuditMiddleware_RecordsTheActorTheCommandCarried(t *testing.T) {
	pool := inboxFixture(t)
	pubsub := auditRouter(t, pool, "test.actor.topic", nil)

	actorID := uuid.New()
	action := "test.actor." + uuid.NewString() + ".v1"
	msg := message.NewMessage(uuid.NewString(), []byte(`{}`))
	msg.Metadata.Set(messaging.HeaderEventType, action)
	msg.Metadata.Set(messaging.HeaderActorID, actorID.String())
	if err := pubsub.Publish("test.actor.topic", msg); err != nil {
		t.Fatalf("publish: %v", err)
	}

	userID, ok, _ := waitAuditRow(t, pool, action)
	if userID == nil || *userID != actorID {
		t.Fatalf("audit user_id = %v, want %s", userID, actorID)
	}
	if !ok {
		t.Fatal("succeeded = false, want true")
	}
}

func TestAuditMiddleware_AMessageWithoutAnActorHasNoUser(t *testing.T) {
	pool := inboxFixture(t)
	pubsub := auditRouter(t, pool, "test.noactor.topic", nil)

	action := "test.noactor." + uuid.NewString() + ".v1"
	msg := message.NewMessage(uuid.NewString(), []byte(`{}`))
	msg.Metadata.Set(messaging.HeaderEventType, action)
	msg.Metadata.Set(messaging.HeaderActorID, "not-a-uuid")
	if err := pubsub.Publish("test.noactor.topic", msg); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if userID, _, _ := waitAuditRow(t, pool, action); userID != nil {
		t.Fatalf("audit user_id = %v, want NULL for a malformed actor", userID)
	}
}

func TestAuditMiddleware_FailureStoresAStableCodeNotTheRawError(t *testing.T) {
	pool := inboxFixture(t)
	raw := errors.New(`ERROR: duplicate key value violates unique constraint "uq_secret_internal_name" (SQLSTATE 23505)`)
	pubsub := auditRouter(t, pool, "test.failcode.topic", raw)

	action := "test.failcode." + uuid.NewString() + ".v1"
	msg := message.NewMessage(uuid.NewString(), []byte(`{}`))
	msg.Metadata.Set(messaging.HeaderEventType, action)
	if err := pubsub.Publish("test.failcode.topic", msg); err != nil {
		t.Fatalf("publish: %v", err)
	}

	_, ok, failure := waitAuditRow(t, pool, action)
	if ok {
		t.Fatal("succeeded = true for a failed handler")
	}
	if failure == nil || *failure != messaging.FailureHandlerFailed {
		t.Fatalf("failure_reason = %v, want %q", failure, messaging.FailureHandlerFailed)
	}
	if strings.Contains(*failure, "uq_secret") || strings.Contains(*failure, "SQLSTATE") {
		t.Fatalf("failure_reason leaks the raw error: %q", *failure)
	}
}
