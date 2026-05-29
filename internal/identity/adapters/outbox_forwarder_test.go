//go:build integration

// arch-test:no-synctest — exercises the outbox-forwarder pump goroutine
// against a real Postgres testcontainer; the polling waits cross the SQL
// driver + Watermill subscriber boundary, neither of which testing/
// synctest's virtual clock can model.

package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Subscriber + drain + waitForCount primitives + the outboxFixture
// helper live in outbox_subscriber_test.go (shared across every
// outbox-observing test in this package per the strict-TDL canon).

func TestOutboxForwarder_PublishesUnforwardedRows(t *testing.T) {
	sharedPG.TruncateAll(t)
	fix := newOutboxFixture(t)

	// Drive the application: register a tenant — this writes one outbox
	// row (TenantRegisteredEvent → identity.tenant_registered.v1).
	tenants := adapters.NewTenantRepository(fix.pool, pg.NewTransactor(fix.pool))
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

	count, err := fix.forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("ForwardOnce: %v", err)
	}
	if count != 1 {
		t.Fatalf("forwarded count: got %d want 1", count)
	}

	got := waitForCount(t, fix.drain, 1, 2*time.Second)
	msg := got[0]
	if msg.Metadata.Get(messaging.HeaderEventType) != "identity.tenant_registered.v1" {
		t.Fatalf("event_type metadata: got %q", msg.Metadata.Get(messaging.HeaderEventType))
	}
	if msg.Metadata.Get(messaging.HeaderTenantID) != tn.ID().String() {
		t.Fatalf("tenant_id metadata: got %q want %q",
			msg.Metadata.Get(messaging.HeaderTenantID), tn.ID().String())
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
	sharedPG.TruncateAll(t)
	fix := newOutboxFixture(t)

	tenants := adapters.NewTenantRepository(fix.pool, pg.NewTransactor(fix.pool))
	full := ids.NewV7().String()
	registerSlug, _ := slug.New("idempotent-" + full[len(full)-8:])
	addr, _ := email.New("idempotent@flow.test")
	tn, _ := tenant.New(tenant.ID(ids.NewV7().String()), registerSlug, "Idempotent Pharma", "IP", addr, testNow)
	if err := tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("Add: %v", err)
	}

	first, err := fix.forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("first ForwardOnce: %v", err)
	}
	if first != 1 {
		t.Fatalf("first pass count: got %d want 1", first)
	}

	// Second pass: row is now forwarded=true, must skip.
	second, err := fix.forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("second ForwardOnce: %v", err)
	}
	if second != 0 {
		t.Fatalf("second pass count: got %d want 0", second)
	}
}

func TestOutboxForwarder_RunStopsOnContextCancel(t *testing.T) {
	sharedPG.TruncateAll(t)
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
