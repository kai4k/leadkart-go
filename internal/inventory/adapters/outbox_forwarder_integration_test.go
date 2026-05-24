//go:build integration

package adapters_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// invDrainSubscriber records every received message; identical shape to
// identity's drainSubscriber but scoped to the inventory test package.
type invDrainSubscriber struct {
	mu       sync.Mutex
	received []*message.Message
}

func (d *invDrainSubscriber) record(msgs <-chan *message.Message) {
	for msg := range msgs {
		d.mu.Lock()
		d.received = append(d.received, msg)
		d.mu.Unlock()
		msg.Ack()
	}
}

func (d *invDrainSubscriber) snapshot() []*message.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*message.Message, len(d.received))
	copy(out, d.received)
	return out
}

func invWaitForCount(t *testing.T, drain *invDrainSubscriber, want int, timeout time.Duration) []*message.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := drain.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("inventory subscriber: timed out waiting for %d messages, got %d",
		want, len(drain.snapshot()))
	return nil
}

func invSilentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestInventoryOutboxForwarder_PublishesProductCreated proves the
// per-module forwarder reads inventory.outbox (not identity's) and
// publishes onto the inventory.events Watermill topic. Reviewer C1:
// the previous build silently orphaned every inventory event because
// the identity forwarder hardcodes identity.outbox.
func TestInventoryOutboxForwarder_PublishesProductCreated(t *testing.T) {
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(invSilentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	msgs, err := pubsub.Subscribe(t.Context(), integrationevents.Topic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drain := &invDrainSubscriber{}
	go drain.record(msgs)

	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, integrationevents.Topic, 0, func() time.Time { return fixedNow })

	// Drive the application: create a product. Same-tx outbox write
	// per ADR 0008.
	actor := membership.ID(ids.NewV7().String())
	p, err := product.New(
		product.ID(ids.NewV7().String()),
		tid, actor,
		product.Spec{
			SKU: "FWD-1", Name: "Forwarder Drug",
			DosageForm: "Tablet", PackSize: "10",
			HSNCode: "3004", GSTRateBps: 1200,
		},
		fixedNow,
	)
	if err != nil {
		t.Fatalf("product.New: %v", err)
	}
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("products.Add: %v", err)
	}

	// Drain.
	count, err := forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("ForwardOnce: %v", err)
	}
	if count != 1 {
		t.Fatalf("forwarded count: got %d want 1", count)
	}

	got := invWaitForCount(t, drain, 1, 2*time.Second)
	msg := got[0]
	if msg.Metadata.Get("event_type") != "inventory.product_created.v1" {
		t.Fatalf("event_type metadata: got %q want inventory.product_created.v1",
			msg.Metadata.Get("event_type"))
	}
	if msg.Metadata.Get("tenant_id") != tid.String() {
		t.Fatalf("tenant_id metadata: got %q want %q",
			msg.Metadata.Get("tenant_id"), tid.String())
	}
	var payload struct {
		ProductID string `json:"product_id"`
		TenantID  string `json:"tenant_id"`
		SKU       string `json:"sku"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload.ProductID != p.ID().String() {
		t.Fatalf("payload product_id: got %q want %q",
			payload.ProductID, p.ID().String())
	}
	if payload.SKU != "FWD-1" {
		t.Fatalf("payload sku: got %q", payload.SKU)
	}
}

// TestInventoryOutboxForwarder_IsIdempotent_OnSecondPass — second
// ForwardOnce against an already-drained outbox returns 0.
func TestInventoryOutboxForwarder_IsIdempotent_OnSecondPass(t *testing.T) {
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(invSilentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	msgs, err := pubsub.Subscribe(t.Context(), integrationevents.Topic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drain := &invDrainSubscriber{}
	go drain.record(msgs)

	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, integrationevents.Topic, 0, func() time.Time { return fixedNow })

	actor := membership.ID(ids.NewV7().String())
	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "IDEM-1", Name: "Idem", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	first, err := forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("first ForwardOnce: %v", err)
	}
	if first != 1 {
		t.Fatalf("first pass: got %d want 1", first)
	}
	second, err := forwarder.ForwardOnce(t.Context())
	if err != nil {
		t.Fatalf("second ForwardOnce: %v", err)
	}
	if second != 0 {
		t.Fatalf("second pass (already forwarded): got %d want 0", second)
	}
}
