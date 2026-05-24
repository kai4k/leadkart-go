//go:build integration

package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const outboxTopic = "identity.events"

// forwarderFixedNow is the deterministic instant identity outbox-forwarder
// integration tests pass into NewOutboxForwarder per the clock-injection
// refactor — replaces the prior implicit clock.Now() reliance.
var forwarderFixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// drainSubscriber records every received message into a slice. Unlike
// the production subscriber which persists state, this one is purely
// in-memory + assertion-friendly.
type drainSubscriber struct {
	mu       sync.Mutex
	received []*message.Message
}

func (d *drainSubscriber) record(msgs <-chan *message.Message) {
	for msg := range msgs {
		d.mu.Lock()
		d.received = append(d.received, msg)
		d.mu.Unlock()
		msg.Ack()
	}
}

func (d *drainSubscriber) snapshot() []*message.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*message.Message, len(d.received))
	copy(out, d.received)
	return out
}

// waitForCount polls the subscriber's slice until len ≥ want or timeout.
// Returns the recorded slice on success; t.Fatal on timeout.
func waitForCount(t *testing.T, drain *drainSubscriber, want int, timeout time.Duration) []*message.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := drain.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("subscriber: timed out waiting for %d messages, got %d", want, len(drain.snapshot()))
	return nil
}

func TestOutboxForwarder_PublishesUnforwardedRows(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)

	// Watermill in-process pub/sub — close at test end so the goroutine
	// inside drainSubscriber.record exits cleanly.
	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	msgs, err := pubsub.Subscribe(t.Context(), outboxTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drain := &drainSubscriber{}
	go drain.record(msgs)

	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, outboxTopic, 0, func() time.Time { return forwarderFixedNow })

	// Drive the application: register a tenant — this writes one outbox
	// row (TenantRegisteredEvent → identity.tenant_registered.v1).
	full := ids.NewV7().String()
	registerSlug, err := slug.New("forwarder-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("forward@flow.test")
	tn, err := tenant.New(tenant.ID(ids.NewV7().String()), registerSlug, "Forward Pharma", "FP", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenant Add: %v", err)
	}

	// Drain.
	count, err := forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("ForwardOnce: %v", err)
	}
	if count != 1 {
		t.Fatalf("forwarded count: got %d want 1", count)
	}

	got := waitForCount(t, drain, 1, 2*time.Second)

	msg := got[0]
	if msg.Metadata.Get("event_type") != "identity.tenant_registered.v1" {
		t.Fatalf("event_type metadata: got %q", msg.Metadata.Get("event_type"))
	}
	if msg.Metadata.Get("tenant_id") != tn.ID().String() {
		t.Fatalf("tenant_id metadata: got %q want %q",
			msg.Metadata.Get("tenant_id"), tn.ID().String())
	}

	// Payload is the marshaled integration-event V1 record — primitive
	// snake_case wire shape per integrationevents.TenantRegisteredV1.
	var payload struct {
		TenantID string `json:"tenant_id"`
		Slug     string `json:"slug"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload.TenantID != tn.ID().String() {
		t.Fatalf("payload tenant_id: got %q want %q", payload.TenantID, tn.ID().String())
	}
	if payload.Slug != tn.Slug().String() {
		t.Fatalf("payload slug: got %q want %q", payload.Slug, tn.Slug().String())
	}
}

func TestOutboxForwarder_IsIdempotent_OnSecondPass(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	msgs, err := pubsub.Subscribe(t.Context(), outboxTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drain := &drainSubscriber{}
	go drain.record(msgs)

	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, outboxTopic, 0, func() time.Time { return forwarderFixedNow })

	full := ids.NewV7().String()
	registerSlug, _ := slug.New("idempotent-" + full[len(full)-8:])
	addr, _ := email.New("idempotent@flow.test")
	tn, _ := tenant.New(tenant.ID(ids.NewV7().String()), registerSlug, "Idempotent Pharma", "IP", addr, testNow)
	if err := tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("Add: %v", err)
	}

	first, err := forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("first ForwardOnce: %v", err)
	}
	if first != 1 {
		t.Fatalf("first pass count: got %d want 1", first)
	}

	// Second pass: row is now forwarded=true, must skip.
	second, err := forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("second ForwardOnce: %v", err)
	}
	if second != 0 {
		t.Fatalf("second pass count: got %d want 0", second)
	}
}

func TestOutboxForwarder_RunStopsOnContextCancel(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, outboxTopic, 0, func() time.Time { return forwarderFixedNow })

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		var lastErr error
		forwarder.Run(ctx, 50*time.Millisecond, 10*time.Millisecond, func(err error) {
			// Forwarder errors on ctx-cancel are tolerated; we just want
			// Run to return.
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				lastErr = err
			}
		})
		_ = lastErr
		close(done)
	}()

	select {
	case <-done:
		// expected — Run respected ctx cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s after context cancel")
	}
}

// silentSlog returns a slog.Logger that writes nothing — keeps test
// runs quiet. Watermill's NewSlogLogger wraps it.
func silentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
