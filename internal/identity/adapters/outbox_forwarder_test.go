//go:build integration

// arch-test:no-timeout-needed — these tests do a single repository write
// then a synchronous read of common.outbox via messagingtest.DrainOutbox
// (no async subscriber goroutine to deadlock); execution is bounded by the
// shared pgtest container + the package-level `go test -timeout`. A per-test
// context.WithTimeout would be belt-and-suspenders.

// outbox_forwarder_test.go — producer→outbox contract for identity.
//
// Post-ADR-0064: the per-module forwarder is gone; a single library
// Watermill Forwarder (cmd/worker) drains common.outbox. These tests
// verify the PRODUCER side — a repository write enqueues the correct
// enveloped row in the same tx — via messagingtest.DrainOutbox.
// The forwarder hop is library code and not re-verified here.

package adapters_test

import (
	"encoding/json"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	identityevents "github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// TestOutbox_TenantRegistered_EnqueuesEnvelopedEvent verifies that
// registering a tenant writes exactly one enveloped row to common.outbox
// with the correct event_type, tenant_id, destination topic, and V1 payload.
func TestOutbox_TenantRegistered_EnqueuesEnvelopedEvent(t *testing.T) {
	sharedPG.TruncateAll(t)
	fix := newOutboxFixture(t)

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

	msgs := fix.forwardAndWait(t, 1)
	msg := msgs[0]
	if got := msg.Metadata.Get(messaging.HeaderEventType); got != "identity.tenant_registered.v1" {
		t.Fatalf("event_type metadata: got %q", got)
	}
	if got := msg.Metadata.Get(messaging.HeaderTenantID); got != tn.ID().String() {
		t.Fatalf("tenant_id metadata: got %q want %q", got, tn.ID().String())
	}

	// Payload is the marshaled V1 record (snake_case per integrationevents.TenantRegisteredV1).
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

// TestOutbox_TenantRegistered_EnqueuesExactlyOnce proves no duplicate enqueue
// per registration. Drain idempotency is the forwarder's DeleteOnAck concern
// (ADR 0064).
func TestOutbox_TenantRegistered_EnqueuesExactlyOnce(t *testing.T) {
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

	// forwardAndWait asserts exactly one row on the identity topic.
	msgs := fix.forwardAndWait(t, 1)
	if got := msgs[0].Metadata.Get(messaging.HeaderEventType); got != "identity.tenant_registered.v1" {
		t.Fatalf("event_type: got %q", got)
	}
	// Assert destination-topic contract.
	rows := drainOutboxRows(t, fix)
	if rows[0].DestinationTopic != identityevents.Topic {
		t.Fatalf("destination topic: got %q want %q", rows[0].DestinationTopic, identityevents.Topic)
	}
}
