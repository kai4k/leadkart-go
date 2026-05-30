//go:build integration

// outbox_forwarder_integration_test.go — producer→outbox contract for
// inventory. Post-ADR-0064: the per-module hand-rolled forwarder is gone;
// one library Watermill Forwarder (cmd/worker) drains the shared
// common.outbox relay. This verifies the PRODUCER side — a repository write
// enqueues the right enveloped event to common.outbox — via
// messagingtest.DrainOutbox (relay read + envelope unwrap). The forwarder
// hop is library code, not re-verified here.

package adapters_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// TestInventoryOutbox_ProductCreated_EnqueuesEnvelopedEvent proves creating
// a product writes one enveloped row to common.outbox carrying the
// inventory destination topic, event_type + tenant metadata, and the V1
// payload the forwarder will republish.
func TestInventoryOutbox_ProductCreated_EnqueuesEnvelopedEvent(t *testing.T) {
	// arch-test:no-parallel — cross-tenant scan; uses TruncateAll
	sharedPG.TruncateAll(t)
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)

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

	// The shared common.outbox relay (ADR 0064) also carries the
	// tenant-seed's identity event; assert on THIS module's event by
	// filtering to the inventory destination topic.
	rows := messagingtest.RowsForTopic(messagingtest.DrainOutbox(t.Context(), t, pool), integrationevents.Topic)
	if len(rows) != 1 {
		t.Fatalf("inventory outbox rows: got %d want 1", len(rows))
	}
	row := rows[0]
	if row.DestinationTopic != integrationevents.Topic {
		t.Fatalf("destination topic: got %q want %q", row.DestinationTopic, integrationevents.Topic)
	}
	if got := row.Message.Metadata.Get(messaging.HeaderEventType); got != "inventory.product_created.v1" {
		t.Fatalf("event_type: got %q want inventory.product_created.v1", got)
	}
	if got := row.Message.Metadata.Get(messaging.HeaderTenantID); got != tid.String() {
		t.Fatalf("tenant_id: got %q want %q", got, tid.String())
	}
	var payload struct {
		ProductID string `json:"product_id"`
		TenantID  string `json:"tenant_id"`
		SKU       string `json:"sku"`
	}
	if err := json.Unmarshal(row.Message.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload.ProductID != p.ID().String() {
		t.Fatalf("payload product_id: got %q want %q", payload.ProductID, p.ID().String())
	}
	if payload.SKU != "FWD-1" {
		t.Fatalf("payload sku: got %q", payload.SKU)
	}
}

// TestInventoryOutbox_ProductCreated_EnqueuesExactlyOnce proves the
// producer writes exactly one outbox row per create — no duplicate enqueue.
// (Replaces the old "second ForwardOnce returns 0" test: drain idempotency
// is now the library forwarder's DeleteOnAck concern, per ADR 0064.)
func TestInventoryOutbox_ProductCreated_EnqueuesExactlyOnce(t *testing.T) {
	// arch-test:no-parallel — cross-tenant scan; uses TruncateAll
	sharedPG.TruncateAll(t)
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)

	actor := membership.ID(ids.NewV7().String())
	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "IDEM-1", Name: "Idem", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Filter to the inventory destination topic — the shared relay also
	// holds the tenant-seed's identity event (ADR 0064).
	rows := messagingtest.RowsForTopic(messagingtest.DrainOutbox(t.Context(), t, pool), integrationevents.Topic)
	if len(rows) != 1 {
		t.Fatalf("inventory outbox rows: got %d want 1 (exactly-once enqueue)", len(rows))
	}
}
