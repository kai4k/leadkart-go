//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(), every wait below is
// bounded by waitFor or a fixed settle window, and every direct handler call carries its own
// deadline.
//
// arch-test:no-synctest — the proofs run the real outbox forwarder and routers against a real
// Postgres; virtual time cannot model that.
//
// arch-test:raw-sql-justified — the proofs read and age rows of three modules (batches,
// movements, orders, notes, the shared outbox) to compare the whole world before and after a
// dispatch; no API shows the movement ledger by order, and none moves a batch expiry. The one
// trigger is created as the database owner to make a single statement fail on purpose.

// ADR 0072: the dispatch of a consignment note moves stock, order and note in ONE Postgres
// transaction, or changes nothing. The proofs below run the real adapters on a real Postgres
// and, wherever an event is involved, the production bus (outbox forwarder and router).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	dispatchcommand "github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventorycommand "github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

// ----- the rig ----------------------------------------------------------------------------

type txRig struct {
	f     e2eFixture
	tid   string
	actor membership.ID
	tok   string
	stack *fulfilmentStack
	bus   *busRecorder
}

func newTxRig(t *testing.T, name string) txRig {
	t.Helper()
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, name)
	stack := startStockStack(t, f.Pool)
	rec, _ := stockRecorders.Load(f.Pool)
	return txRig{f: f, tid: reg.TenantID, actor: membership.ID(reg.MembershipID), tok: reg.AccessToken, stack: stack, bus: rec.(*busRecorder)}
}

// tenantCtx is the context the release subscriber's handler is called with (its unit of work
// binds the company from it).
func (r txRig) tenantCtx(t *testing.T) context.Context {
	t.Helper()
	return tenancy.WithID(t.Context(), tenancy.ID(r.tid))
}

func (r txRig) inCompany(t *testing.T, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := pg.NewTransactor(r.f.Pool).WithinTxPgxTenant(t.Context(), r.tid, func(_ context.Context, tx pgx.Tx) error { return fn(tx) }); err != nil {
		t.Fatalf("company statement: %v", err)
	}
}

func validBatchExpiry() time.Time {
	now := time.Now().UTC()
	return expiry.MonthEnd(now.Year(), now.Month()+6)
}

// batch adds a valid batch holding qty units and returns its id.
func (r txRig) batch(t *testing.T, productID, number string, qty int) string {
	t.Helper()
	add := invAddBatch(t, r.f, r.tok, productID, number, validBatchExpiry(), map[string]any{"initial_quantity": qty})
	if add.status != http.StatusCreated {
		t.Fatalf("add batch %s: %d %s", number, add.status, add.body)
	}
	id, _ := invObj(t, add.body)["batch_id"].(string)
	return id
}

type readyOrder struct {
	orderID, orderPath, noteID string
	batches                    []string
}

func line(productID, sku string) ordersports.LineItemDto {
	return ordersports.LineItemDto{ProductID: productID, SKU: sku, Quantity: 10, UnitMrpPaise: 10000, UnitSalePaise: 9000, GstRateBps: 1200}
}

// readyOrder takes an order of the given lines to the point where the goods are still in the
// warehouse: confirmed and reserved (one hold per batch in batches), packed, invoiced, its
// consignment note created and pending. The stock stack reserves; nothing else runs on its own.
func (r txRig) readyOrder(t *testing.T, items []ordersports.LineItemDto, batches []string) readyOrder {
	t.Helper()
	q := r.f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations", r.tok, ordersports.CreateQuotationRequest{CustomerLeadID: uuid.NewString(), Items: items})
	if q.status != http.StatusCreated {
		t.Fatalf("create quotation: %d %s", q.status, q.body)
	}
	quoteID, _ := invObj(t, q.body)["quotation_id"].(string)
	ap := r.f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations/"+quoteID+"/approve", r.tok, nil)
	if ap.status != http.StatusOK {
		t.Fatalf("approve: %d %s", ap.status, ap.body)
	}
	o := readyOrder{batches: batches}
	o.orderID, _ = invObj(t, ap.body)["order_id"].(string)
	o.orderPath = "/api/v1/orders/orders/" + o.orderID
	if x := r.f.authedJSON(t, http.MethodPost, o.orderPath+"/token-payment", r.tok, ordersports.RecordTokenPaymentRequest{Method: "upi", AmountPaise: 25000, ExternalReference: "UTR-" + uuid.NewString()}); x.status != http.StatusNoContent {
		t.Fatalf("token payment: %d %s", x.status, x.body)
	}
	if x := r.f.authedJSON(t, http.MethodPost, o.orderPath+"/confirm", r.tok, nil); x.status != http.StatusNoContent {
		t.Fatalf("confirm: %d %s", x.status, x.body)
	}
	waitFor(t, "the confirmed order reserves its stock", func() bool {
		n := 0
		r.inCompany(t, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM inventory.stock_movements WHERE source_reference = $1 AND type = 'reservation'`, o.orderID).Scan(&n)
		})
		return n == len(batches)
	})
	if x := r.f.authedJSON(t, http.MethodPost, o.orderPath+"/pack", r.tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}); x.status != http.StatusNoContent {
		t.Fatalf("pack: %d %s", x.status, x.body)
	}
	if x := r.f.authedJSON(t, http.MethodPost, o.orderPath+"/invoice", r.tok, nil); x.status != http.StatusOK {
		t.Fatalf("invoice: %d %s", x.status, x.body)
	}
	mk := r.f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", r.tok, dispatchports.CreateConsignmentNoteRequest{OrderID: o.orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500})
	if mk.status != http.StatusCreated {
		t.Fatalf("note: %d %s", mk.status, mk.body)
	}
	o.noteID, _ = invObj(t, mk.body)["consignment_note_id"].(string)
	return o
}

// oneLine is the common order: 10 units of one fresh product, on one fresh batch of qty units.
func (r txRig) oneLine(t *testing.T, sku string, qty int) (readyOrder, string, string) {
	t.Helper()
	productID := invCreateProduct(t, r.f, r.tok, sku, nil)
	batchID := r.batch(t, productID, "LOT-"+sku, qty)
	return r.readyOrder(t, []ordersports.LineItemDto{line(productID, sku)}, []string{batchID}), productID, batchID
}

func (r txRig) dispatch(t *testing.T, noteID, docket string) httpResp {
	t.Helper()
	return r.f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes/"+noteID+"/dispatch", r.tok, dispatchports.MarkDispatchedRequest{DocketNumber: docket})
}

// world is everything a dispatch may change in the database, as one comparable string: the
// batches' stock, the order's movements, the order and the note. What it published is seen on the
// bus (busRecorder).
func (r txRig) world(t *testing.T, o readyOrder) string {
	t.Helper()
	var b strings.Builder
	r.inCompany(t, func(tx pgx.Tx) error {
		rows, err := tx.Query(t.Context(), `SELECT quantity_on_hand, quantity_reserved, version FROM inventory.batches WHERE id::text = ANY($1) ORDER BY id`, o.batches)
		if err != nil {
			return err
		}
		for rows.Next() {
			var onHand, reserved, version int64
			if err := rows.Scan(&onHand, &reserved, &version); err != nil {
				return err
			}
			fmt.Fprintf(&b, "batch[%d/%d v%d] ", onHand, reserved, version)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		mrows, err := tx.Query(t.Context(), `SELECT type, count(*), sum(quantity) FROM inventory.stock_movements WHERE source_reference = $1 GROUP BY type ORDER BY type`, o.orderID)
		if err != nil {
			return err
		}
		for mrows.Next() {
			var typ string
			var n, sum int64
			if err := mrows.Scan(&typ, &n, &sum); err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s[%dx %d] ", typ, n, sum)
		}
		mrows.Close()
		if err := mrows.Err(); err != nil {
			return err
		}
		var state string
		var dispatched bool
		if err := tx.QueryRow(t.Context(), `SELECT state, dispatched_at IS NOT NULL FROM orders.orders WHERE id = $1`, o.orderID).Scan(&state, &dispatched); err != nil {
			return err
		}
		fmt.Fprintf(&b, "order[%s dispatched=%v] ", state, dispatched)
		var status, docket string
		if err := tx.QueryRow(t.Context(), `SELECT status, docket_number FROM dispatch.consignment_notes WHERE id = $1`, o.noteID).Scan(&status, &docket); err != nil {
			return err
		}
		fmt.Fprintf(&b, "note[%s %q] ", status, docket)
		return nil
	})
	return b.String()
}

// stockOf is a batch's on-hand and reserved units.
func (r txRig) stockOf(t *testing.T, batchID string) (onHand, reserved int64) {
	t.Helper()
	r.inCompany(t, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT quantity_on_hand, quantity_reserved FROM inventory.batches WHERE id = $1`, batchID).Scan(&onHand, &reserved)
	})
	return onHand, reserved
}

// movementsOf is the sum of an order's movement rows of one type.
func (r txRig) movementsOf(t *testing.T, orderID, typ string) (rows, sum int64) {
	t.Helper()
	r.inCompany(t, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT count(*), COALESCE(sum(quantity), 0) FROM inventory.stock_movements WHERE source_reference = $1 AND type = $2`, orderID, typ).Scan(&rows, &sum)
	})
	return rows, sum
}

// ageBatch moves a batch's printed expiry into the past.
func (r txRig) ageBatch(t *testing.T, batchID string) {
	t.Helper()
	now := time.Now().UTC()
	r.inCompany(t, func(tx pgx.Tx) error {
		tag, err := tx.Exec(t.Context(), `UPDATE inventory.batches SET expiry_date = $2 WHERE id = $1`, batchID, expiry.MonthEnd(now.Year(), now.Month()-1))
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("the batch was not aged")
		}
		return err
	})
}

// ----- the dispatch handler wired by hand, so a step can be made to fail ------------------

type dispatchHooks struct {
	// failAfterOrder / failAfterStock make the step fail AFTER it did its work in the
	// transaction: the proof is that its work is undone with the rest.
	failAfterOrder error
	failAfterStock error
	failAfterNote  error
}

type orderThenFail struct {
	inner dispatchcommand.OrderShipper
	fail  error
}

func (o orderThenFail) MarkOrderDispatched(ctx context.Context, tid tenant.ID, orderID string, actor membership.ID) error {
	if err := o.inner.MarkOrderDispatched(ctx, tid, orderID, actor); err != nil {
		return err
	}
	return o.fail
}

type stockThenFail struct {
	inner dispatchcommand.StockShipper
	fail  error
}

func (s stockThenFail) ShipOrderStock(ctx context.Context, tid tenant.ID, orderID string, actor membership.ID) error {
	if err := s.inner.ShipOrderStock(ctx, tid, orderID, actor); err != nil {
		return err
	}
	return s.fail
}

// noteThenFail runs the note's own UpdateByID (row lock, persist, event into the outbox) and
// then fails: the note's row and its outbox row must go with everything else.
type noteThenFail struct {
	consignmentnote.Repository
	fail error
}

func (n noteThenFail) UpdateByID(ctx context.Context, tid tenant.ID, id consignmentnote.ID, fn func(*consignmentnote.ConsignmentNote) (bool, error)) error {
	if err := n.Repository.UpdateByID(ctx, tid, id, fn); err != nil {
		return err
	}
	return n.fail
}

// buildTxDispatch is the PRODUCTION dispatch wiring (buildDispatchAppWith, the constructor
// buildDispatchApp calls) on the given pool, with optional failures injected through its hooks.
func buildTxDispatch(pool *pgxpool.Pool, h dispatchHooks) dispatchcommand.MarkDispatchedHandler {
	hooks := dispatchWiringHooks{}
	if h.failAfterNote != nil {
		hooks.notes = func(n consignmentnote.Repository) consignmentnote.Repository {
			return noteThenFail{Repository: n, fail: h.failAfterNote}
		}
	}
	if h.failAfterOrder != nil {
		hooks.order = func(o dispatchcommand.OrderShipper) dispatchcommand.OrderShipper {
			return orderThenFail{inner: o, fail: h.failAfterOrder}
		}
	}
	if h.failAfterStock != nil {
		hooks.stock = func(s dispatchcommand.StockShipper) dispatchcommand.StockShipper {
			return stockThenFail{inner: s, fail: h.failAfterStock}
		}
	}
	return buildDispatchAppWith(pool, time.Now, hooks).Commands.MarkDispatched
}

func (r txRig) handle(t *testing.T, h dispatchcommand.MarkDispatchedHandler, noteID, docket string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return h.Handle(ctx, dispatchcommand.MarkDispatchedCommand{
		TenantID: tenant.ID(r.tid), ConsignmentNoteID: consignmentnote.ID(noteID), DocketNumber: docket, TransitionedByMembership: r.actor,
	})
}

// ----- proofs 1, 3, 9: one dispatch over HTTP ---------------------------------------------

func TestE2E_Dispatch_OneTransaction_SuccessRetryAndRefusals(t *testing.T) {
	r := newTxRig(t, "dispatchtx1")

	t.Run("1: success changes stock, both movements, order, note and their events together", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		o, _, batchID := r.oneLine(t, "TX-OK", 20)
		before := r.world(t, o)

		if x := r.dispatch(t, o.noteID, "BDX-1"); x.status != http.StatusNoContent {
			t.Fatalf("dispatch: %d %s", x.status, x.body)
		}

		if onHand, reserved := r.stockOf(t, batchID); onHand != 10 || reserved != 0 {
			t.Fatalf("stock on hand %d reserved %d; want 10 and 0", onHand, reserved)
		}
		if n, sum := r.movementsOf(t, o.orderID, "release"); n != 1 || sum != 10 {
			t.Fatalf("release movements %d (sum %d); want 1 (10)", n, sum)
		}
		if n, sum := r.movementsOf(t, o.orderID, "outbound"); n != 1 || sum != -10 {
			t.Fatalf("outbound movements %d (sum %d); want 1 (-10)", n, sum)
		}
		after := r.world(t, o)
		if !strings.Contains(after, "order[dispatched dispatched=true]") || !strings.Contains(after, `note[dispatched "BDX-1"]`) {
			t.Fatalf("world after: %s (before: %s)", after, before)
		}
		// The events written in the same transaction reach the bus: the two stock movements
		// (release, outbound) and the note's dispatched event. The order's own transition
		// writes no outbox row: Orders suppresses its bare AdvancedEvent (events.go), exactly
		// as it does for every order transition but confirm, pack and cancel.
		waitFor(t, "the release, the outbound and the note's event reach the bus", func() bool {
			return r.bus.movementsOf(o.orderID, "release") == 1 && r.bus.movementsOf(o.orderID, "outbound") == 1 && r.bus.notesOf(o.noteID) == 1
		})
	})

	t.Run("3: the same docket again is a success that deducts nothing a second time", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		o, _, batchID := r.oneLine(t, "TX-RETRY", 20)
		if x := r.dispatch(t, o.noteID, "BDX-2"); x.status != http.StatusNoContent {
			t.Fatalf("dispatch: %d %s", x.status, x.body)
		}
		first := r.world(t, o)

		if x := r.dispatch(t, o.noteID, "BDX-2"); x.status != http.StatusNoContent {
			t.Fatalf("same-docket retry: %d %s; want 204", x.status, x.body)
		}
		if second := r.world(t, o); second != first {
			t.Fatalf("a retry changed the world:\n first: %s\nsecond: %s", first, second)
		}
		if x := r.dispatch(t, o.noteID, "OTHER-DOCKET"); x.status == http.StatusNoContent {
			t.Fatalf("another docket on a dispatched note was accepted: %d %s", x.status, x.body)
		}
		if again := r.world(t, o); again != first {
			t.Fatalf("a refused retry changed the world:\n first: %s\n again: %s", first, again)
		}
		if onHand, reserved := r.stockOf(t, batchID); onHand != 10 || reserved != 0 {
			t.Fatalf("stock on hand %d reserved %d; want 10 and 0 (one deduction)", onHand, reserved)
		}
		waitFor(t, "the first dispatch's events reach the bus", func() bool { return r.bus.notesOf(o.noteID) >= 1 })
		time.Sleep(1500 * time.Millisecond) // arch-test:wait-justified — negative proof: a retry that publishes nothing has no event to wait for
		if r.bus.notesOf(o.noteID) != 1 || r.bus.movementsOf(o.orderID, "outbound") != 1 || r.bus.movementsOf(o.orderID, "release") != 1 {
			t.Fatalf("events on the bus: %d note, %d outbound, %d release; want one of each (the retries published nothing)", r.bus.notesOf(o.noteID), r.bus.movementsOf(o.orderID, "outbound"), r.bus.movementsOf(o.orderID, "release"))
		}
	})

	t.Run("9: no hold is a 409 order_stock_not_held, an expired hold a 409 order_stock_expired, and nothing changes", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		// An order that holds nothing: its hold was released.
		o, _, _ := r.oneLine(t, "TX-NOHOLD", 20)
		rel := inventorycommand.NewReleaseOrderStockHandler(pg.NewTransactor(r.f.Pool),
			inventoryadapters.NewBatchRepository(r.f.Pool, pg.NewTransactor(r.f.Pool)),
			inventoryadapters.NewStockMovementRepository(r.f.Pool, pg.NewTransactor(r.f.Pool)),
			inventoryadapters.NewStockMovementRepository(r.f.Pool, pg.NewTransactor(r.f.Pool)),
			time.Now, func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) })
		if _, err := rel.Handle(r.tenantCtx(t), inventorycommand.ReleaseOrderStockCommand{TenantID: tenant.ID(r.tid), OrderID: o.orderID, ActorMembershipID: r.actor, Reason: "test"}); err != nil {
			t.Fatalf("release: %v", err)
		}
		before := r.world(t, o)
		x := r.dispatch(t, o.noteID, "BDX-3")
		if x.status != http.StatusConflict || errorCode(t, x.body) != "order_stock_not_held" {
			t.Fatalf("dispatch with no hold: %d %s; want 409 order_stock_not_held", x.status, x.body)
		}
		if after := r.world(t, o); after != before {
			t.Fatalf("a refused dispatch changed the world:\n before: %s\n  after: %s", before, after)
		}

		// A hold on a batch whose printed month has ended.
		e, _, batchID := r.oneLine(t, "TX-EXPIRED", 20)
		r.ageBatch(t, batchID)
		before = r.world(t, e)
		x = r.dispatch(t, e.noteID, "BDX-4")
		if x.status != http.StatusConflict || errorCode(t, x.body) != "order_stock_expired" {
			t.Fatalf("dispatch of an expired hold: %d %s; want 409 order_stock_expired", x.status, x.body)
		}
		if after := r.world(t, e); after != before {
			t.Fatalf("a refused dispatch changed the world:\n before: %s\n  after: %s", before, after)
		}
	})
}

// ----- proof 2: a failure at each step leaves everything as it was ------------------------

func TestE2E_Dispatch_OneTransaction_AFailureAtAnyStepChangesNothing(t *testing.T) {
	r := newTxRig(t, "dispatchtx2")
	boom := errors.New("injected failure")

	run := func(t *testing.T, name string, hooks dispatchHooks, wrapOutbox bool) {
		t.Helper()
		o, _, batchID := r.oneLine(t, "TX-FAIL-"+name, 20)
		before := r.world(t, o)
		if wrapOutbox {
			failOutboxInserts(t, r.f.Pool, true)
			defer failOutboxInserts(t, r.f.Pool, false)
		}
		err := r.handle(t, buildTxDispatch(r.f.Pool, hooks), o.noteID, "BDX-FAIL")
		if err == nil {
			t.Fatalf("%s: the dispatch succeeded; want the injected failure", name)
		}
		if wrapOutbox {
			failOutboxInserts(t, r.f.Pool, false)
		}
		if after := r.world(t, o); after != before {
			t.Fatalf("%s: a failed dispatch changed the world:\n before: %s\n  after: %s", name, before, after)
		}
		if onHand, reserved := r.stockOf(t, batchID); onHand != 20 || reserved != 10 {
			t.Fatalf("%s: stock on hand %d reserved %d; want 20 and 10", name, onHand, reserved)
		}
		time.Sleep(1500 * time.Millisecond) // arch-test:wait-justified — negative proof: a rolled-back dispatch publishes nothing, so there is no event to wait for
		if r.bus.notesOf(o.noteID) != 0 || r.bus.movementsOf(o.orderID, "release") != 0 || r.bus.movementsOf(o.orderID, "outbound") != 0 {
			t.Fatalf("%s: a failed dispatch published events: %d note, %d release, %d outbound", name, r.bus.notesOf(o.noteID), r.bus.movementsOf(o.orderID, "release"), r.bus.movementsOf(o.orderID, "outbound"))
		}
		// And the dispatch still works afterwards: nothing was left half done.
		if err := r.handle(t, buildTxDispatch(r.f.Pool, dispatchHooks{}), o.noteID, "BDX-FAIL"); err != nil {
			t.Fatalf("%s: the dispatch after the failure: %v", name, err)
		}
		if onHand, reserved := r.stockOf(t, batchID); onHand != 10 || reserved != 0 {
			t.Fatalf("%s: stock after the retry on hand %d reserved %d; want 10 and 0", name, onHand, reserved)
		}
		waitFor(t, name+": the retry's events reach the bus, once", func() bool {
			return r.bus.notesOf(o.noteID) == 1 && r.bus.movementsOf(o.orderID, "release") == 1 && r.bus.movementsOf(o.orderID, "outbound") == 1
		})
	}

	t.Run("the stock step fails after it moved stock", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		run(t, "stock", dispatchHooks{failAfterStock: boom}, false)
	})
	t.Run("the order step fails after it moved the order", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		run(t, "order", dispatchHooks{failAfterOrder: boom}, false)
	})
	t.Run("the note fails after it was saved with its event", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		run(t, "note", dispatchHooks{failAfterNote: boom}, false)
	})
	t.Run("the outbox refuses a row", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		run(t, "outbox", dispatchHooks{}, true)
	})
}

// failOutboxInserts installs (or removes) a trigger, as the database owner, that makes every
// insert into the shared outbox fail: a real failure of the one statement that writes the
// events of the dispatch.
func failOutboxInserts(t *testing.T, pool *pgxpool.Pool, on bool) {
	t.Helper()
	dsn, ok := ownerDSNs.Load(pool)
	if !ok {
		t.Fatal("no owner connection recorded for this fixture")
	}
	conn, err := pgx.Connect(t.Context(), dsn.(string))
	if err != nil {
		t.Fatalf("owner connect: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	stmts := []string{`DROP TRIGGER IF EXISTS lk_fail_outbox ON common.outbox`, `DROP FUNCTION IF EXISTS common.lk_fail_outbox()`}
	if on {
		stmts = []string{
			`CREATE OR REPLACE FUNCTION common.lk_fail_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected outbox failure'; END $$`,
			`CREATE TRIGGER lk_fail_outbox BEFORE INSERT ON common.outbox FOR EACH ROW EXECUTE FUNCTION common.lk_fail_outbox()`,
		}
	}
	for _, s := range stmts {
		if _, err := conn.Exec(context.WithoutCancel(t.Context()), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// ----- proof 4: two dispatches of one note deduct once ------------------------------------

func TestE2E_Dispatch_OneTransaction_TwoConcurrentDispatchesOfOneNoteDeductOnce(t *testing.T) {
	r := newTxRig(t, "dispatchtx4")
	for round := 0; round < 6; round++ {
		o, _, batchID := r.oneLine(t, fmt.Sprintf("TX-CONC-%d", round), 20)
		const workers = 4
		var wg sync.WaitGroup
		start := make(chan struct{})
		statuses := make(chan int, workers)
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp, err := r.f.tryDo(t.Context(), http.MethodPost, "/api/v1/dispatch/consignment-notes/"+o.noteID+"/dispatch", r.tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-C"}, nil)
				errs <- err
				statuses <- resp.status
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		close(statuses)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: request: %v", round, err)
			}
		}
		for s := range statuses {
			if s != http.StatusNoContent {
				t.Fatalf("round %d: status %d; want 204 for every caller of the same docket", round, s)
			}
		}
		if onHand, reserved := r.stockOf(t, batchID); onHand != 10 || reserved != 0 {
			t.Fatalf("round %d: stock on hand %d reserved %d; want 10 and 0 (one deduction)", round, onHand, reserved)
		}
		if n, _ := r.movementsOf(t, o.orderID, "outbound"); n != 1 {
			t.Fatalf("round %d: %d outbound movements; want 1", round, n)
		}
	}
}

// ----- proof 5: dispatch racing cancel -----------------------------------------------------

func TestE2E_Dispatch_OneTransaction_DispatchRacingCancelIsShippedOrReleasedNeverBothNeverNeither(t *testing.T) {
	r := newTxRig(t, "dispatchtx5")
	shipped, released := 0, 0
	for round := 0; round < 8; round++ {
		o, _, batchID := r.oneLine(t, fmt.Sprintf("TX-RACE-%d", round), 20)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var dStatus, cStatus int
		var dErr, cErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			resp, err := r.f.tryDo(t.Context(), http.MethodPost, "/api/v1/dispatch/consignment-notes/"+o.noteID+"/dispatch", r.tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-R"}, nil)
			dStatus, dErr = resp.status, err
		}()
		go func() {
			defer wg.Done()
			<-start
			resp, err := r.f.tryDo(t.Context(), http.MethodPost, o.orderPath+"/cancel", r.tok, ordersports.CancelOrderRequest{Reason: "changed mind"}, nil)
			cStatus, cErr = resp.status, err
		}()
		close(start)
		wg.Wait()
		if dErr != nil || cErr != nil {
			t.Fatalf("round %d: requests: %v %v", round, dErr, cErr)
		}
		if cStatus != http.StatusNoContent {
			t.Fatalf("round %d: cancel %d; want 204", round, cStatus)
		}
		// The release of a cancelled order is an event: wait for the hold to settle.
		waitFor(t, "the hold is drained", func() bool { _, reserved := r.stockOf(t, batchID); return reserved == 0 })
		onHand, _ := r.stockOf(t, batchID)
		relRows, relSum := r.movementsOf(t, o.orderID, "release")
		outRows, _ := r.movementsOf(t, o.orderID, "outbound")
		switch dStatus {
		case http.StatusNoContent: // shipped (a cancel after it keeps the deduction until returns, G8)
			shipped++
			// Never both: the order left `invoiced` BEFORE it was cancelled. A cancelled order
			// that shipped has no dispatched_at, or one later than its cancelled_at.
			var dispatchedFirst bool
			r.inCompany(t, func(tx pgx.Tx) error {
				return tx.QueryRow(t.Context(), `SELECT dispatched_at IS NOT NULL AND cancelled_at IS NOT NULL AND dispatched_at <= cancelled_at FROM orders.orders WHERE id = $1`, o.orderID).Scan(&dispatchedFirst)
			})
			if !dispatchedFirst {
				t.Fatalf("round %d: the dispatch answered 204 but the order was not dispatched before it was cancelled", round)
			}
			if onHand != 10 || outRows != 1 || relRows != 1 || relSum != 10 {
				t.Fatalf("round %d: shipped, but on hand %d outbound rows %d release rows %d (sum %d); want 10, 1, 1 (10)", round, onHand, outRows, relRows, relSum)
			}
		case http.StatusConflict: // the cancel won: the hold was released, nothing left
			released++
			if onHand != 20 || outRows != 0 || relRows != 1 || relSum != 10 {
				t.Fatalf("round %d: released, but on hand %d outbound rows %d release rows %d (sum %d); want 20, 0, 1 (10)", round, onHand, outRows, relRows, relSum)
			}
		default:
			t.Fatalf("round %d: dispatch %d; want 204 or 409", round, dStatus)
		}
	}
	t.Logf("8 rounds: %d shipped, %d released", shipped, released)

	t.Run("the two orders of events, one after the other", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		// Dispatch, then cancel: shipped; the cancel releases nothing (stock stays out until returns, G8).
		a, _, aBatch := r.oneLine(t, "TX-AFTER", 20)
		if x := r.dispatch(t, a.noteID, "BDX-A"); x.status != http.StatusNoContent {
			t.Fatalf("dispatch: %d %s", x.status, x.body)
		}
		if x := r.f.authedJSON(t, http.MethodPost, a.orderPath+"/cancel", r.tok, ordersports.CancelOrderRequest{Reason: "returned"}); x.status != http.StatusNoContent {
			t.Fatalf("cancel after dispatch: %d %s", x.status, x.body)
		}
		time.Sleep(1500 * time.Millisecond) // arch-test:wait-justified — negative proof: the release of a drained hold changes nothing, so there is no event to wait for
		if onHand, reserved := r.stockOf(t, aBatch); onHand != 10 || reserved != 0 {
			t.Fatalf("cancel after dispatch: on hand %d reserved %d; want 10 and 0 (still deducted)", onHand, reserved)
		}
		// Cancel, then dispatch: refused, the hold released.
		c, _, cBatch := r.oneLine(t, "TX-BEFORE", 20)
		if x := r.f.authedJSON(t, http.MethodPost, c.orderPath+"/cancel", r.tok, ordersports.CancelOrderRequest{Reason: "changed mind"}); x.status != http.StatusNoContent {
			t.Fatalf("cancel: %d %s", x.status, x.body)
		}
		waitFor(t, "the cancel releases the hold", func() bool { _, reserved := r.stockOf(t, cBatch); return reserved == 0 })
		if x := r.dispatch(t, c.noteID, "BDX-B"); x.status != http.StatusConflict || errorCode(t, x.body) != "order_not_dispatchable" {
			t.Fatalf("dispatch after cancel: %d %s; want 409 order_not_dispatchable", x.status, x.body)
		}
		if onHand, reserved := r.stockOf(t, cBatch); onHand != 20 || reserved != 0 {
			t.Fatalf("dispatch after cancel: on hand %d reserved %d; want 20 and 0", onHand, reserved)
		}
	})
}

// ----- proof 6: workers sharing a batch ---------------------------------------------------

func TestE2E_Dispatch_OneTransaction_ConcurrentDispatchAndReleasesLeaveTheOtherOrdersHold(t *testing.T) {
	r := newTxRig(t, "dispatchtx6")
	tx := pg.NewTransactor(r.f.Pool)
	movements := inventoryadapters.NewStockMovementRepository(r.f.Pool, tx)
	release := inventorycommand.NewReleaseOrderStockHandler(tx, inventoryadapters.NewBatchRepository(r.f.Pool, tx), movements, movements,
		time.Now, func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) })
	dispatcher := buildTxDispatch(r.f.Pool, dispatchHooks{})

	for round := 0; round < 8; round++ {
		sku := fmt.Sprintf("TX-SHARE-%d", round)
		productID := invCreateProduct(t, r.f, r.tok, sku, nil)
		batchID := r.batch(t, productID, "LOT-"+sku, 40)
		a := r.readyOrder(t, []ordersports.LineItemDto{line(productID, sku)}, []string{batchID})
		b := r.readyOrder(t, []ordersports.LineItemDto{line(productID, sku)}, []string{batchID})
		if _, reserved := r.stockOf(t, batchID); reserved != 20 {
			t.Fatalf("round %d: reserved %d; want both orders' 10", round, reserved)
		}

		const workers = 4
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, workers+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := r.handle(t, dispatcher, a.noteID, "BDX-S")
			if errors.Is(err, dispatchcommand.ErrStockNotHeld) {
				err = nil // a release got to the hold first: refused, nothing shipped
			}
			errs <- err
		}()
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := release.Handle(r.tenantCtx(t), inventorycommand.ReleaseOrderStockCommand{TenantID: tenant.ID(r.tid), OrderID: a.orderID, ActorMembershipID: r.actor, Reason: "order cancelled"})
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}

		// A's hold left exactly once (one release row of 10, shipped or not), B's is intact.
		if n, sum := r.movementsOf(t, a.orderID, "release"); n != 1 || sum != 10 {
			t.Fatalf("round %d: order A: %d release rows (sum %d); want its hold drained once", round, n, sum)
		}
		if n, sum := r.movementsOf(t, b.orderID, "release"); n != 0 || sum != 0 {
			t.Fatalf("round %d: order B: %d release rows (sum %d); want none", round, n, sum)
		}
		if _, reserved := r.stockOf(t, batchID); reserved != 10 {
			t.Fatalf("round %d: reserved %d; want B's 10 intact", round, reserved)
		}
		onHand, _ := r.stockOf(t, batchID)
		if outRows, _ := r.movementsOf(t, a.orderID, "outbound"); (outRows == 1 && onHand != 30) || (outRows == 0 && onHand != 40) {
			t.Fatalf("round %d: on hand %d with %d outbound rows for A", round, onHand, outRows)
		}
	}
}

// ----- proof 7: opposite line order, no deadlock ------------------------------------------

func TestE2E_Dispatch_OneTransaction_OrdersLockingTheSameBatchesInOppositeLineOrderDoNotDeadlock(t *testing.T) {
	r := newTxRig(t, "dispatchtx7")
	dispatcher := buildTxDispatch(r.f.Pool, dispatchHooks{})
	for round := 0; round < 6; round++ {
		skuX, skuY := fmt.Sprintf("TX-LOCK-X%d", round), fmt.Sprintf("TX-LOCK-Y%d", round)
		px, py := invCreateProduct(t, r.f, r.tok, skuX, nil), invCreateProduct(t, r.f, r.tok, skuY, nil)
		bx, by := r.batch(t, px, "LOT-"+skuX, 40), r.batch(t, py, "LOT-"+skuY, 40)
		// The same two batches, the lines in opposite order.
		first := r.readyOrder(t, []ordersports.LineItemDto{line(px, skuX), line(py, skuY)}, []string{bx, by})
		second := r.readyOrder(t, []ordersports.LineItemDto{line(py, skuY), line(px, skuX)}, []string{bx, by})

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 2)
		for _, o := range []readyOrder{first, second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- r.handle(t, dispatcher, o.noteID, "BDX-L")
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v (a deadlock is SQLSTATE 40P01)", round, err)
			}
		}
		for _, b := range []string{bx, by} {
			if onHand, reserved := r.stockOf(t, b); onHand != 20 || reserved != 0 {
				t.Fatalf("round %d: batch on hand %d reserved %d; want 20 and 0", round, onHand, reserved)
			}
		}
	}
}

// ----- proof 8: one connection is enough --------------------------------------------------

func TestE2E_Dispatch_OneTransaction_CompletesOnAOneConnectionPool(t *testing.T) {
	r := newTxRig(t, "dispatchtx8")
	o, _, batchID := r.oneLine(t, "TX-ONECONN", 20)

	cfg := r.f.Pool.Config().Copy()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("one-connection pool: %v", err)
	}
	t.Cleanup(one.Close)

	// A step that opened a transaction of its own (Transactor.WithinTx and WithinTenantTx
	// never join the one in ctx) would wait for a second connection that does not exist, and
	// this call would run into its deadline.
	if err := r.handle(t, buildTxDispatch(one, dispatchHooks{}), o.noteID, "BDX-1C"); err != nil {
		t.Fatalf("dispatch on a one-connection pool: %v", err)
	}
	if onHand, reserved := r.stockOf(t, batchID); onHand != 10 || reserved != 0 {
		t.Fatalf("stock on hand %d reserved %d; want 10 and 0", onHand, reserved)
	}

	// The refusal paths give the connection back too.
	e, _, eBatch := r.oneLine(t, "TX-ONECONN-EXP", 20)
	r.ageBatch(t, eBatch)
	if err := r.handle(t, buildTxDispatch(one, dispatchHooks{}), e.noteID, "BDX-1C"); !errors.Is(err, dispatchcommand.ErrStockExpired) {
		t.Fatalf("dispatch of an expired hold on a one-connection pool: %v; want ErrStockExpired", err)
	}
	if err := r.handle(t, buildTxDispatch(one, dispatchHooks{}), e.noteID, "BDX-1C"); !errors.Is(err, dispatchcommand.ErrStockExpired) {
		t.Fatalf("second refusal: %v; want ErrStockExpired (the connection was returned)", err)
	}
}

// ----- proof 10: the pack touches no stock ------------------------------------------------

func TestE2E_Dispatch_OneTransaction_ALateOrReplayedOrderPackedChangesNoStock(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "dispatchtx10")
	tok := reg.AccessToken
	rec := &busRecorder{}
	stack := startFulfilmentStackWith(t, f.Pool, fulfilmentOptions{extra: rec.handlers()})
	productID := invCreateProduct(t, f, tok, "TX-PACKED", nil)
	add := invAddBatch(t, f, tok, productID, "LOT-1", validBatchExpiry(), map[string]any{"initial_quantity": 20})
	if add.status != http.StatusCreated {
		t.Fatalf("add batch: %d %s", add.status, add.body)
	}
	batchID, _ := invObj(t, add.body)["batch_id"].(string)
	stock := func() (onHand, reserved float64) {
		b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID, tok, nil).body)
		onHand, _ = b["quantity_on_hand"].(float64)
		reserved, _ = b["quantity_reserved"].(float64)
		return onHand, reserved
	}
	orderID, orderPath := confirmedOrderOn(t, f, tok, productID, "TX-PACKED")
	waitFor(t, "the confirmed order reserves 10 units", func() bool { _, r := stock(); return r == 10 })

	// The pack's events are delivered late.
	stack.pause()
	if x := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}); x.status != http.StatusNoContent {
		t.Fatalf("pack: %d %s", x.status, x.body)
	}
	if onHand, reserved := stock(); onHand != 20 || reserved != 10 {
		t.Fatalf("after the pack: on hand %v reserved %v; want 20 and 10", onHand, reserved)
	}
	stack.resume()
	waitFor(t, "the late pack event invoiced the order and made its note", func() bool {
		var ord ordersports.OrderDto
		if err := json.Unmarshal(f.authedJSON(t, http.MethodGet, orderPath, tok, nil).body, &ord); err != nil {
			return false
		}
		return ord.State == "invoiced"
	})
	if onHand, reserved := stock(); onHand != 20 || reserved != 10 {
		t.Fatalf("after the late pack event: on hand %v reserved %v; want 20 and 10", onHand, reserved)
	}

	// The same pack event delivered again.
	if err := pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return messaging.PublishOutbox(ctx, tx, ordersevents.Topic, uuid.MustParse(reg.TenantID), []ordersevents.Event{ordersevents.OrderPackedV1{
			OrderID: orderID, TenantID: reg.TenantID, BoxCount: 2, WeightGrams: 1500, CarrierName: "BlueDart",
			PackedAtUTC: time.Now().UTC(), PackedByMembershipID: reg.MembershipID,
		}})
	}); err != nil {
		t.Fatalf("publish the pack event again: %v", err)
	}
	waitFor(t, "the pack event is delivered twice (late, then again)", func() bool { return rec.packedOf(orderID) == 2 })
	time.Sleep(1500 * time.Millisecond) // arch-test:wait-justified — negative proof: a delivery that must not change stock has no event to wait for
	if onHand, reserved := stock(); onHand != 20 || reserved != 10 {
		t.Fatalf("after the replayed pack event: on hand %v reserved %v; want 20 and 10", onHand, reserved)
	}
	var movements int
	if err := pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(_ context.Context, tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT count(*) FROM inventory.stock_movements WHERE source_reference = $1 AND type IN ('release','outbound')`, orderID).Scan(&movements)
	}); err != nil {
		t.Fatalf("movements: %v", err)
	}
	if movements != 0 {
		t.Fatalf("%d release/outbound movements before any dispatch; want none", movements)
	}
}
