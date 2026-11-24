//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup;
// per-request HTTP uses t.Context().

// End-to-end HTTP contract test for the Dispatch surface: consignment-note
// create (manual route) → reads → carrier status lifecycle through the real
// ServeMux + authz gates.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
	dispatchevents "github.com/leadkart/leadkart-go/internal/dispatch/integrationevents"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	inventoryevents "github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
	inventorysubscribers "github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

// TestE2E_Dispatch_ConsignmentLifecycle drives create → get-by-order →
// dispatch(docket) → in-transit → delivered, then proves terminal-state 409
// and idempotent re-create (200 already_existed).
func TestE2E_Dispatch_ConsignmentLifecycle(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "dispatchflow")
	tok := owner.AccessToken
	orderID := invoicedOrderID(t, f, tok, "DISP-FLOW")

	// Create the slot.
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
		OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create: status %d body %s", resp.status, resp.body)
	}
	var created dispatchports.CreateConsignmentNoteResponse
	if err := json.Unmarshal(resp.body, &created); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	if created.AlreadyExisted {
		t.Fatal("fresh create flagged already_existed")
	}

	// Idempotent re-create for the same order → 200 + already_existed.
	resp = f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
		OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
	})
	if resp.status != http.StatusOK {
		t.Fatalf("re-create: status %d want 200 body %s", resp.status, resp.body)
	}

	// Get by order.
	resp = f.authedJSON(t, http.MethodGet, "/api/v1/dispatch/consignment-notes?order_id="+orderID, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get by order: status %d body %s", resp.status, resp.body)
	}
	var note dispatchports.ConsignmentNoteDto
	if err := json.Unmarshal(resp.body, &note); err != nil {
		t.Fatalf("get decode: %v", err)
	}
	if note.Status != "pending" || note.ID != created.ConsignmentNoteID {
		t.Fatalf("note status=%s id=%s want pending/%s", note.Status, note.ID, created.ConsignmentNoteID)
	}

	base := "/api/v1/dispatch/consignment-notes/" + note.ID

	// pending → dispatched (docket) → in_transit → delivered.
	resp = f.authedJSON(t, http.MethodPost, base+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BD-12345"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("dispatch: status %d body %s", resp.status, resp.body)
	}
	for _, step := range []string{"/in-transit", "/delivered"} {
		resp = f.authedJSON(t, http.MethodPost, base+step, tok, nil)
		if resp.status != http.StatusNoContent {
			t.Fatalf("%s: status %d body %s", step, resp.status, resp.body)
		}
	}

	resp = f.authedJSON(t, http.MethodGet, base, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get by id: status %d body %s", resp.status, resp.body)
	}
	if err := json.Unmarshal(resp.body, &note); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if note.Status != "delivered" || note.DocketNumber != "BD-12345" {
		t.Fatalf("note status=%s docket=%s want delivered/BD-12345", note.Status, note.DocketNumber)
	}

	// Terminal guard: failing a delivered note → 409.
	resp = f.authedJSON(t, http.MethodPost, base+"/failed", tok, dispatchports.MarkFailedRequest{Reason: "late"})
	if resp.status != http.StatusConflict {
		t.Fatalf("fail delivered: status %d want 409 body %s", resp.status, resp.body)
	}

	// Anonymous → 401.
	resp = f.authedJSON(t, http.MethodGet, base, "", nil)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("anon get: status %d want 401", resp.status)
	}
}

// stockStacks remembers the pools that already run the stock stack, so a test that builds
// several orders starts it once.
var stockStacks sync.Map

// stockRecorders holds, per pool, the observer of the bus the stock stack was started with.
var stockRecorders sync.Map

// busRecorder observes, through the production bus, the events a dispatch publishes: the
// stock movements and the note's dispatched event (and the pack events). Nothing reads the
// outbox table (ADR 0062 Amendment 1).
type busRecorder struct {
	mu        sync.Mutex
	movements []inventoryevents.StockMovementLoggedV1
	notes     []dispatchevents.ConsignmentNoteDispatchedV1
	packed    []ordersevents.OrderPackedV1
}

func (b *busRecorder) handlers() []cqrs.EventHandler {
	return []cqrs.EventHandler{
		cqrs.NewEventHandler("test.recorder.StockMovementLogged", func(_ context.Context, e *inventoryevents.StockMovementLoggedV1) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.movements = append(b.movements, *e)
			return nil
		}),
		cqrs.NewEventHandler("test.recorder.ConsignmentNoteDispatched", func(_ context.Context, e *dispatchevents.ConsignmentNoteDispatchedV1) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.notes = append(b.notes, *e)
			return nil
		}),
		cqrs.NewEventHandler("test.recorder.OrderPacked", func(_ context.Context, e *ordersevents.OrderPackedV1) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.packed = append(b.packed, *e)
			return nil
		}),
	}
}

// movementsOf counts the stock movement events of one order and type seen on the bus.
func (b *busRecorder) movementsOf(orderID, typ string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.movements {
		if m.SourceReference != nil && *m.SourceReference == orderID && m.Type == typ {
			n++
		}
	}
	return n
}

// notesOf counts the dispatched events of one note seen on the bus.
func (b *busRecorder) notesOf(noteID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.notes {
		if e.ConsignmentNoteID.String() == noteID {
			n++
		}
	}
	return n
}

// packedOf counts the pack events of one order seen on the bus.
func (b *busRecorder) packedOf(orderID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.packed {
		if e.OrderID == orderID {
			n++
		}
	}
	return n
}

// startStockStack runs the production outbox forwarder and router with the two stock
// handlers only: a confirmed order reserves its stock and a cancelled order releases it, and
// nothing else happens on its own (no auto-invoice, no consignment note), so a test drives the
// invoice and the note through the API. It is started once per pool.
func startStockStack(t *testing.T, pool *pgxpool.Pool) *fulfilmentStack {
	t.Helper()
	if v, loaded := stockStacks.Load(pool); loaded {
		return v.(*fulfilmentStack)
	}
	rec := &busRecorder{}
	stack := startFulfilmentStackWith(t, pool, fulfilmentOptions{
		keep: func(name string) bool {
			return name == inventorysubscribers.HandlerReserveStock || name == inventorysubscribers.HandlerReleaseStock
		},
		extra: rec.handlers(),
	})
	stockStacks.Store(pool, stack)
	stockRecorders.Store(pool, rec)
	return stack
}

// invoicedOrderID makes a real order that holds 10 units of valid stock and has a tax invoice
// (quote, approve, token payment, confirm and its reservation, pack, manual invoice) and
// returns its id: a consignment note is dispatched only for an order with an invoice (review
// G2d B1) and a hold to ship (ADR 0072).
func invoicedOrderID(t *testing.T, f e2eFixture, tok, sku string) string {
	t.Helper()
	startStockStack(t, f.Pool)
	productID := invCreateProduct(t, f, tok, sku, nil)
	now := time.Now().UTC()
	add := invAddBatch(t, f, tok, productID, "LOT-"+sku, expiry.MonthEnd(now.Year(), now.Month()+6), map[string]any{"initial_quantity": 20})
	if add.status != http.StatusCreated {
		t.Fatalf("add batch: %d %s", add.status, add.body)
	}
	batchID, _ := invObj(t, add.body)["batch_id"].(string)
	orderID, orderPath := confirmedOrderOn(t, f, tok, productID, sku)
	waitFor(t, "the confirmed order reserves 10 units", func() bool {
		b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID, tok, nil).body)
		r, _ := b["quantity_reserved"].(float64)
		return r == 10
	})
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}); r.status != http.StatusNoContent {
		t.Fatalf("pack: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/invoice", tok, nil); r.status != http.StatusOK {
		t.Fatalf("invoice: %d %s", r.status, r.body)
	}
	return orderID
}
