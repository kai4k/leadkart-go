//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(), and
// every wait below is bounded by waitFor or a fixed settle window.
//
// arch-test:no-synctest — the proof runs the real outbox forwarder and routers
// against a real Postgres; virtual time cannot model that.
//
// arch-test:raw-sql-justified — the proof reads dispatch rows (a different module's table)
// as the company.

// Review G2c B2: a timed-out order event, delivered again after the order moved on,
// corrupted stock (a cancelled order held 10 units for good; a pending consignment note on a
// cancelled order). The pack no longer moves stock at all (ADR 0072: the dispatch does, in
// one transaction), so there is no consume event to be late. The order-lifecycle handlers
// are no longer replayed automatically: their dead letters stay for a person, and the replay
// job leaves stock and documents exactly as they are. Every handler here times out because
// it is held behind a gate, the same ending as a handler stuck behind a lock; the router, the
// bus, the forwarder and the replayer are the production ones.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	dispatchsubscribers "github.com/leadkart/leadkart-go/internal/dispatch/ports/subscribers"
	inventorysubscribers "github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
	orderssubscribers "github.com/leadkart/leadkart-go/internal/orders/ports/subscribers"
)

// gates holds named handlers behind a gate: while a gate is closed the handler waits for
// its attempt timeout and fails with it, the way a handler stuck behind a lock does.
type gates struct {
	mu    sync.Mutex
	state map[string]*atomic.Bool
}

func newGates(names ...string) *gates {
	g := &gates{state: map[string]*atomic.Bool{}}
	for _, n := range names {
		g.state[n] = &atomic.Bool{}
	}
	return g
}

func (g *gates) set(open bool, names ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range names {
		g.state[n].Store(open)
	}
}

func (g *gates) wrap(h cqrs.EventHandler) cqrs.EventHandler {
	g.mu.Lock()
	open := g.state[h.HandlerName()]
	g.mu.Unlock()
	if open == nil {
		return h
	}
	return gatedHandler{EventHandler: h, open: open}
}

type gatedHandler struct {
	cqrs.EventHandler
	open *atomic.Bool
}

func (g gatedHandler) Handle(ctx context.Context, event any) error {
	if !g.open.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return g.EventHandler.Handle(ctx, event)
}

func TestE2E_Orders_ALateEventIsNeverReplayedOntoAnOrderThatMovedOn(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "lateevent")
	tok := reg.AccessToken
	const (
		reserve  = inventorysubscribers.HandlerReserveStock
		release  = inventorysubscribers.HandlerReleaseStock
		invoice  = orderssubscribers.HandlerAutoInvoice
		noteSlot = dispatchsubscribers.HandlerCreateConsignmentNote
	)
	g := newGates(reserve, release, invoice, noteSlot)
	g.set(true, reserve, release, invoice, noteSlot)
	stack := startFulfilmentStackWith(t, f.Pool, fulfilmentOptions{wrap: g.wrap, handlerTimeout: 300 * time.Millisecond})

	inCompany := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(_ context.Context, tx pgx.Tx) error { return fn(tx) }); err != nil {
			t.Fatalf("company statement: %v", err)
		}
	}
	type world struct {
		onHand, reserved float64
		state, invoiceID string
		notes            int
	}
	type scenario struct {
		productID, batchID, orderID, orderPath string
	}
	look := func(s scenario) world {
		t.Helper()
		b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+s.batchID, tok, nil).body)
		var w world
		w.onHand, _ = b["quantity_on_hand"].(float64)
		w.reserved, _ = b["quantity_reserved"].(float64)
		var ord ordersports.OrderDto
		if err := json.Unmarshal(f.authedJSON(t, http.MethodGet, s.orderPath, tok, nil).body, &ord); err != nil {
			t.Fatalf("decode order: %v", err)
		}
		w.state, w.invoiceID = ord.State, ord.InvoiceID
		inCompany(func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM dispatch.consignment_notes WHERE order_id = $1`, s.orderID).Scan(&w.notes)
		})
		return w
	}
	newScenario := func(sku string) scenario {
		t.Helper()
		productID := invCreateProduct(t, f, tok, sku, nil)
		now := time.Now().UTC()
		add := invAddBatch(t, f, tok, productID, "LOT-"+sku, expiry.MonthEnd(now.Year(), now.Month()+6), map[string]any{"initial_quantity": 20})
		if add.status != http.StatusCreated {
			t.Fatalf("add batch: %d %s", add.status, add.body)
		}
		batchID, _ := invObj(t, add.body)["batch_id"].(string)
		return scenario{productID: productID, batchID: batchID}
	}
	confirm := func(s *scenario, sku string) {
		t.Helper()
		s.orderID, s.orderPath = confirmedOrderOn(t, f, tok, s.productID, sku)
	}
	pack := func(s scenario) {
		t.Helper()
		if r := f.authedJSON(t, http.MethodPost, s.orderPath+"/pack", tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}); r.status != http.StatusNoContent {
			t.Fatalf("pack: %d %s", r.status, r.body)
		}
	}
	cancel := func(s scenario) {
		t.Helper()
		if r := f.authedJSON(t, http.MethodPost, s.orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: "changed mind"}); r.status != http.StatusNoContent {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
	}
	deadLetters := func(h string) int { return messagingtest.DeadLetterCount(t.Context(), t, f.Pool, h) }
	// untouchedByTheReplayJob opens the gates, runs the replay job a day later and shows that
	// nothing was delivered again: no replay row, and the books and documents as they were.
	untouchedByTheReplayJob := func(s scenario, handlers ...string) {
		t.Helper()
		before := look(s)
		g.set(true, reserve, release, invoice, noteSlot)
		replayed, err := stack.replayer.ReplayDue(t.Context(), time.Now().Add(24*time.Hour), 100)
		if err != nil || replayed != 0 {
			t.Fatalf("the replay job replayed %d, %v; want none (order-lifecycle handlers are never replayed)", replayed, err)
		}
		time.Sleep(1500 * time.Millisecond) // arch-test:wait-justified — negative proof: a delivery that must not happen has no event to wait for
		if after := look(s); after != before {
			t.Fatalf("the replay job changed the order's world: before %+v after %+v", before, after)
		}
		for _, h := range handlers {
			if n := messagingtest.ReplayCount(t.Context(), t, f.Pool, h); n != 0 {
				t.Fatalf("%s: %d replays recorded, want 0", h, n)
			}
		}
	}

	t.Run("reserve times out and the order is cancelled: no hold appears", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one set of gates; each runs to completion first.
		s := newScenario("LATE-1")
		g.set(false, reserve)
		base := deadLetters(reserve)
		confirm(&s, "LATE-1")
		waitFor(t, "the reserve is dead-lettered", func() bool { return deadLetters(reserve) == base+1 })
		cancel(s)
		untouchedByTheReplayJob(s, reserve)
		if w := look(s); w.reserved != 0 || w.onHand != 20 || w.state != "cancelled" {
			t.Fatalf("world %+v; want on hand 20, nothing reserved, cancelled", w)
		}
	})

	t.Run("reserve times out and the order is packed: the goods cannot leave, nothing is deducted", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one set of gates; each runs to completion first.
		s := newScenario("LATE-2")
		g.set(false, reserve)
		base := deadLetters(reserve)
		confirm(&s, "LATE-2")
		waitFor(t, "the reserve is dead-lettered", func() bool { return deadLetters(reserve) == base+1 })
		pack(s) // the known gap before round G5: an order with no hold can still be packed
		g.set(true, reserve)
		waitFor(t, "the order is invoiced", func() bool { return look(s).state == "invoiced" })
		var noteID string
		waitFor(t, "the consignment note exists", func() bool {
			found := false
			inCompany(func(tx pgx.Tx) error {
				found = tx.QueryRow(t.Context(), `SELECT id::text FROM dispatch.consignment_notes WHERE order_id = $1`, s.orderID).Scan(&noteID) == nil
				return nil
			})
			return found
		})
		// Goods never leave without a deduction (ADR 0072): with no hold the dispatch is refused.
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes/"+noteID+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-LATE"}); r.status != http.StatusConflict || errorCode(t, r.body) != "order_stock_not_held" {
			t.Fatalf("dispatch of an order that holds nothing: %d %s; want 409 order_stock_not_held", r.status, r.body)
		}
		untouchedByTheReplayJob(s, reserve)
		if w := look(s); w.reserved != 0 || w.onHand != 20 || w.state != "invoiced" {
			t.Fatalf("world %+v; want on hand 20, nothing reserved, still invoiced", w)
		}
	})

	t.Run("the release times out: it is replayed and frees the hold", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one set of gates; each runs to completion first.
		s := newScenario("LATE-3")
		confirm(&s, "LATE-3")
		waitFor(t, "the confirmed order reserves 10 units", func() bool { return look(s).reserved == 10 })
		g.set(false, release, invoice, noteSlot)
		rBase, iBase, nBase := deadLetters(release), deadLetters(invoice), deadLetters(noteSlot)
		pack(s)
		waitFor(t, "auto-invoice and the note are dead-lettered", func() bool {
			return deadLetters(invoice) == iBase+1 && deadLetters(noteSlot) == nBase+1
		})
		cancel(s)
		waitFor(t, "the release is dead-lettered", func() bool { return deadLetters(release) == rBase+1 })
		// Review G2d B3: the release IS replayed (cancellation is terminal and a release only
		// drains what is still held), so the cancelled order's hold is freed. The invoice and
		// the note are not: nothing else is delivered again.
		g.set(true, reserve, release, invoice, noteSlot)
		replayed, err := stack.replayer.ReplayDue(t.Context(), time.Now().Add(24*time.Hour), 100)
		if err != nil || replayed != 1 {
			t.Fatalf("the replay job replayed %d, %v; want exactly the release", replayed, err)
		}
		waitFor(t, "the replayed release frees the hold", func() bool { return look(s).reserved == 0 })
		if w := look(s); w.onHand != 20 || w.state != "cancelled" || w.invoiceID != "" || w.notes != 0 {
			t.Fatalf("world %+v; want on hand 20, nothing reserved, cancelled, no invoice, no note", w)
		}
		if n := messagingtest.ReplayCount(t.Context(), t, f.Pool, release); n != 1 {
			t.Fatalf("release replays recorded: %d, want 1", n)
		}
		// A second run does nothing: the dead letter is replayed once, and the others never.
		before := look(s)
		again, err := stack.replayer.ReplayDue(t.Context(), time.Now().Add(48*time.Hour), 100)
		if err != nil || again != 0 {
			t.Fatalf("a second replay run replayed %d, %v; want none", again, err)
		}
		time.Sleep(1500 * time.Millisecond) // arch-test:wait-justified — negative proof: a delivery that must not happen has no event to wait for
		if after := look(s); after != before {
			t.Fatalf("the second replay run changed the world: before %+v after %+v", before, after)
		}
		for _, h := range []string{invoice, noteSlot} {
			if n := messagingtest.ReplayCount(t.Context(), t, f.Pool, h); n != 0 {
				t.Fatalf("%s: %d replays recorded, want 0", h, n)
			}
		}
	})

	t.Run("the consignment note times out and the order is cancelled: no note appears", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one set of gates; each runs to completion first.
		s := newScenario("LATE-4")
		confirm(&s, "LATE-4")
		waitFor(t, "the confirmed order reserves 10 units", func() bool { return look(s).reserved == 10 })
		g.set(false, invoice, noteSlot)
		iBase, nBase := deadLetters(invoice), deadLetters(noteSlot)
		pack(s)
		waitFor(t, "the note and the auto-invoice are dead-lettered", func() bool {
			return deadLetters(noteSlot) == nBase+1 && deadLetters(invoice) == iBase+1
		})
		cancel(s)
		waitFor(t, "the cancel releases the hold (nothing was shipped)", func() bool { return look(s).reserved == 0 })
		untouchedByTheReplayJob(s, noteSlot, invoice)
		if w := look(s); w.notes != 0 || w.state != "cancelled" || w.invoiceID != "" {
			t.Fatalf("world %+v; want cancelled with no invoice and no consignment note", w)
		}
	})
}
