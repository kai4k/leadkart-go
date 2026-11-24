//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(), and
// every wait below is bounded by waitFor or a fixed settle window.
//
// arch-test:no-synctest — the proof runs the real outbox forwarder and routers
// against a real Postgres; virtual time cannot model that.

// arch-test:raw-sql-justified — the proof ages a batch past its printed month (no API moves a batch
// expiry) and reads dispatch rows (a different module's table) as the company.

// Review G2b R1: an order that holds stock past its printed expiry must not be
// packed, invoiced or dispatched. The Inventory consume refused the shipment but
// the invoice and the consignment note ran anyway, so the goods left on paper and
// the books said they had not. The pack is now refused up front.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/cmd/internal/bridges"
	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	dispatchadapters "github.com/leadkart/leadkart-go/internal/dispatch/adapters"
	dispatchcommand "github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	dispatchquery "github.com/leadkart/leadkart-go/internal/dispatch/app/query"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	dispatchsubscribers "github.com/leadkart/leadkart-go/internal/dispatch/ports/subscribers"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventorycommand "github.com/leadkart/leadkart-go/internal/inventory/app/command"
	inventoryquery "github.com/leadkart/leadkart-go/internal/inventory/app/query"
	inventorysellability "github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	inventorysubscribers "github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
	ordersadapters "github.com/leadkart/leadkart-go/internal/orders/adapters"
	orderscommand "github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
	orderssubscribers "github.com/leadkart/leadkart-go/internal/orders/ports/subscribers"
)

// fulfilmentStack is the running fulfilment event path. The forwarder can be stopped
// and started again, so a test can let events sit in the outbox and deliver them late.
type fulfilmentStack struct {
	pause  func()
	resume func()
	// replayer is the dead-letter replayer cmd/worker builds: it replays the handlers
	// this stack's handlers marked replay-safe, and no others.
	replayer *messaging.DeadLetterReplayer
}

// fulfilmentOptions shape the stack for a test that needs handlers to be late.
type fulfilmentOptions struct {
	// wrap may replace a handler (for example with one that blocks until a gate opens).
	wrap func(cqrs.EventHandler) cqrs.EventHandler
	// handlerTimeout is the per-attempt timeout (zero: the router default).
	handlerTimeout time.Duration
	// keep, when set, registers only the handlers it answers true for (by handler name).
	keep func(handlerName string) bool
	// extra handlers are registered after keep is applied (a test's own observers of the bus).
	extra []cqrs.EventHandler
}

// startFulfilmentStack runs the production event path for an order's fulfilment:
// the outbox forwarder and a router with the subscribers cmd/worker registers for
// reserve, consume, auto-invoice and the consignment note, built the way
// cmd/worker builds them, on the bus cmd/worker uses ([messaging.NewPubSub]: a
// publish blocks until a handler has taken the message).
func startFulfilmentStack(t *testing.T, pool *pgxpool.Pool) *fulfilmentStack {
	t.Helper()
	return startFulfilmentStackWith(t, pool, fulfilmentOptions{})
}

func startFulfilmentStackWith(t *testing.T, pool *pgxpool.Pool, opts fulfilmentOptions) *fulfilmentStack {
	t.Helper()
	log := silentLogger()
	tx := pg.NewTransactor(pool)
	s := policyStack{
		pool:           pool,
		pubsub:         messaging.NewPubSub(watermill.NewSlogLogger(log)),
		tx:             tx,
		handlerTimeout: opts.handlerTimeout,
	}
	t.Cleanup(func() { _ = s.pubsub.Close() })

	// Inventory: reserve on confirm, release on cancel (the shipment is Dispatch's transaction).
	batches := inventoryadapters.NewBatchRepository(pool, tx)
	movements := inventoryadapters.NewStockMovementRepository(pool, tx)
	window := inventorysellability.NewResolver(inventoryadapters.NewPolicyReplicaRepository(pool, tx), time.Now)
	newMID := func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) }
	reserve := inventorysubscribers.NewReserveStockSubscriber(
		inventorycommand.NewReserveOrderStockHandler(tx, batches, movements, movements, inventoryadapters.NewOutboxEnqueuer(), window, time.Now, newMID), log)

	// Dispatch: a consignment note slot per packed order.
	dispatchRepo := dispatchadapters.NewConsignmentNoteRepository(pool, tx)
	dispatchPacked := dispatchsubscribers.NewOrderPackedIngestor(
		dispatchcommand.NewCreateConsignmentNoteHandler(tx, dispatchRepo, time.Now,
			func() consignmentnote.ID { return consignmentnote.ID(ids.NewV7().String()) }), log)

	// Orders: auto-invoice on pack, dispatched on the consignment note.
	orderRepo := ordersadapters.NewOrderRepository(pool, tx)
	stockGuard := bridges.HeldStockExpiry{Held: inventoryquery.NewOrderHoldExpiryHandler(
		movements, inventorysellability.NewResolver(inventoryadapters.NewPolicyReplicaRepository(pool, tx), time.Now))}
	invoiceOrder := orderscommand.NewInvoiceOrderHandler(tx, orderRepo, ordersadapters.NewInvoiceRepository(pool, tx),
		ordersadapters.NewInvoiceNumberAllocator(pool), stockGuard, time.Now, func() invoice.ID { return invoice.ID(ids.NewV7().String()) })
	attach := orderscommand.NewAttachConsignmentHandler(orderRepo)

	release := inventorysubscribers.NewReleaseStockSubscriber(
		inventorycommand.NewReleaseOrderStockHandler(tx, batches, movements, movements, time.Now, newMID), log)
	dispatchCancelled := dispatchsubscribers.NewOrderCancelledSubscriber(
		dispatchquery.NewGetConsignmentNoteByOrderHandler(dispatchRepo), dispatchcommand.NewMarkFailedHandler(dispatchRepo, time.Now), log)

	var handlers []cqrs.EventHandler
	handlers = append(handlers, inventorysubscribers.Handlers(reserve, release, nil)...)
	handlers = append(handlers, dispatchsubscribers.Handlers(dispatchPacked, dispatchCancelled)...)
	handlers = append(handlers, orderssubscribers.Handlers(
		orderssubscribers.NewAutoInvoiceSubscriber(invoiceOrder, log),
		orderssubscribers.NewConsignmentCreatedSubscriber(attach, log), nil, nil, nil)...)
	if opts.keep != nil {
		kept := handlers[:0]
		for _, h := range handlers {
			if opts.keep(h.HandlerName()) {
				kept = append(kept, h)
			}
		}
		handlers = kept
	}
	handlers = append(handlers, opts.extra...)
	// The replayer is built from the handlers as registered, before a test wraps any.
	replayer := messaging.NewDeadLetterReplayer(pool, log, messaging.ReplaySafeHandlerNames(handlers))
	if opts.wrap != nil {
		for i, h := range handlers {
			handlers[i] = opts.wrap(h)
		}
	}
	s.runRouter(t, handlers)

	var stop func()
	start := func() {
		forwarder, err := messaging.NewOutboxForwarder(pool, s.pubsub, watermill.NewSlogLogger(log))
		if err != nil {
			t.Fatalf("messaging.NewOutboxForwarder: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := forwarder.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("forwarder: %v", err)
			}
		}()
		stop = func() { cancel(); <-done; _ = forwarder.Close(); stop = func() {} }
	}
	start()
	t.Cleanup(func() { stop() })
	return &fulfilmentStack{pause: func() { stop() }, resume: start, replayer: replayer}
}

// confirmedOrderOn quotes, approves, pays the token and confirms an order of 10 units of
// the product through the API, and returns the order id and its path.
func confirmedOrderOn(t *testing.T, f e2eFixture, tok, productID, sku string) (orderID, orderPath string) {
	t.Helper()
	q := f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations", tok, ordersports.CreateQuotationRequest{
		CustomerLeadID: uuid.NewString(),
		Items:          []ordersports.LineItemDto{{ProductID: productID, SKU: sku, Quantity: 10, UnitMrpPaise: 10000, UnitSalePaise: 9000, GstRateBps: 1200}},
	})
	if q.status != http.StatusCreated {
		t.Fatalf("create quotation: %d %s", q.status, q.body)
	}
	quoteID, _ := invObj(t, q.body)["quotation_id"].(string)
	ap := f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations/"+quoteID+"/approve", tok, nil)
	if ap.status != http.StatusOK {
		t.Fatalf("approve: %d %s", ap.status, ap.body)
	}
	orderID, _ = invObj(t, ap.body)["order_id"].(string)
	orderPath = "/api/v1/orders/orders/" + orderID
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/token-payment", tok, ordersports.RecordTokenPaymentRequest{
		Method: "upi", AmountPaise: 25000, ExternalReference: "UTR-" + uuid.NewString(),
	}); r.status != http.StatusNoContent {
		t.Fatalf("token payment: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/confirm", tok, nil); r.status != http.StatusNoContent {
		t.Fatalf("confirm: %d %s", r.status, r.body)
	}
	return orderID, orderPath
}

func TestE2E_Orders_AnOrderHoldingExpiredStockCannotBePackedInvoicedOrDispatched(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "packexpired")
	tok := reg.AccessToken
	// The tables are row-level secured: direct statements run as the company.
	inCompany := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(_ context.Context, tx pgx.Tx) error { return fn(tx) }); err != nil {
			t.Fatalf("company statement: %v", err)
		}
	}
	productID := invCreateProduct(t, f, tok, "PACK-EXP", nil)
	now := time.Now().UTC()
	add := invAddBatch(t, f, tok, productID, "LOT-1", expiry.MonthEnd(now.Year(), now.Month()+6), map[string]any{"initial_quantity": 20})
	if add.status != http.StatusCreated {
		t.Fatalf("add batch: %d %s", add.status, add.body)
	}
	batchID, _ := invObj(t, add.body)["batch_id"].(string)
	startFulfilmentStack(t, f.Pool)

	// Quote, approve, token payment, confirm: the confirm event reserves 10 of the 20.
	orderID, orderPath := confirmedOrderOn(t, f, tok, productID, "PACK-EXP")
	stock := func() (onHand, reserved float64) {
		b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID, tok, nil).body)
		onHand, _ = b["quantity_on_hand"].(float64)
		reserved, _ = b["quantity_reserved"].(float64)
		return onHand, reserved
	}
	waitFor(t, "the confirmed order reserves 10 units", func() bool { _, r := stock(); return r == 10 })

	// The batch's printed month passes while the units sit reserved.
	last := expiry.MonthEnd(now.Year(), now.Month()-1)
	inCompany(func(tx pgx.Tx) error {
		tag, err := tx.Exec(t.Context(), `UPDATE inventory.batches SET expiry_date = $2 WHERE id = $1`, batchID, last)
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("the batch was not aged")
		}
		return err
	})

	pack := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500})
	if pack.status != http.StatusConflict || errorCode(t, pack.body) != "order_stock_expired" {
		t.Fatalf("pack: %d %s; want 409 order_stock_expired", pack.status, pack.body)
	}

	// Give the worker every chance to run what a packed order would have started.
	time.Sleep(2 * time.Second) // arch-test:wait-justified — negative proof: nothing may happen, so there is no event to poll for; measured window for forwarder + router
	var ord ordersports.OrderDto
	if err := json.Unmarshal(f.authedJSON(t, http.MethodGet, orderPath, tok, nil).body, &ord); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	if ord.State != "confirmed" || ord.InvoiceID != "" {
		t.Fatalf("order state=%q invoice=%q; want confirmed with no invoice", ord.State, ord.InvoiceID)
	}
	if r := f.authedJSON(t, http.MethodGet, orderPath+"/invoice", tok, nil); r.status != http.StatusNotFound {
		t.Fatalf("invoice read: %d %s; want 404", r.status, r.body)
	}
	var notes int
	inCompany(func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT count(*) FROM dispatch.consignment_notes WHERE order_id = $1`, orderID).Scan(&notes)
	})
	if notes != 0 {
		t.Fatalf("%d consignment notes; want none", notes)
	}
	if onHand, reserved := stock(); onHand != 20 || reserved != 10 {
		t.Fatalf("stock on hand %v reserved %v; want 20 and 10, untouched", onHand, reserved)
	}

	// Review G2d B1: a note made by hand for this order cannot be dispatched either: the
	// order has no invoice, and the invoice is refused on expired stock.
	mk := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
		OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
	})
	if mk.status != http.StatusCreated {
		t.Fatalf("manual note: %d %s", mk.status, mk.body)
	}
	noteID, _ := invObj(t, mk.body)["consignment_note_id"].(string)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes/"+noteID+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-1"}); r.status != http.StatusConflict || errorCode(t, r.body) != "order_not_invoiced" {
		t.Fatalf("dispatch of a note on an uninvoiced order: %d %s; want 409 order_not_invoiced", r.status, r.body)
	}

	// The order is not stuck: it can still be cancelled.
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: "stock expired"}); r.status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
}

// Review G2c B1: the guard at the pack is not the last word. An order packed in time has
// its invoice and consignment events delivered later; when the printed month ends in
// between, the invoice must still be refused. The events here sit in the outbox while the
// month ends, and the bus is the production one. The order stays packed and can be cancelled;
// the refused events wait in the dead letters for a person.
func TestE2E_Orders_AnInvoiceEventArrivingAfterTheExpiryIssuesNoInvoice(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "invlate")
	tok := reg.AccessToken
	inCompany := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(_ context.Context, tx pgx.Tx) error { return fn(tx) }); err != nil {
			t.Fatalf("company statement: %v", err)
		}
	}
	productID := invCreateProduct(t, f, tok, "INV-LATE", nil)
	now := time.Now().UTC()
	add := invAddBatch(t, f, tok, productID, "LOT-1", expiry.MonthEnd(now.Year(), now.Month()+6), map[string]any{"initial_quantity": 20})
	if add.status != http.StatusCreated {
		t.Fatalf("add batch: %d %s", add.status, add.body)
	}
	batchID, _ := invObj(t, add.body)["batch_id"].(string)
	stack := startFulfilmentStack(t, f.Pool)

	orderID, orderPath := confirmedOrderOn(t, f, tok, productID, "INV-LATE")
	stock := func() (onHand, reserved float64) {
		b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID, tok, nil).body)
		onHand, _ = b["quantity_on_hand"].(float64)
		reserved, _ = b["quantity_reserved"].(float64)
		return onHand, reserved
	}
	waitFor(t, "the confirmed order reserves 10 units", func() bool { _, r := stock(); return r == 10 })

	// The pack is valid today, but its events are not delivered: the forwarder is stopped.
	stack.pause()
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}); r.status != http.StatusNoContent {
		t.Fatalf("pack: %d %s", r.status, r.body)
	}
	// The printed month ends, then the events arrive.
	last := expiry.MonthEnd(now.Year(), now.Month()-1)
	inCompany(func(tx pgx.Tx) error {
		tag, err := tx.Exec(t.Context(), `UPDATE inventory.batches SET expiry_date = $2 WHERE id = $1`, batchID, last)
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("the batch was not aged")
		}
		return err
	})
	stack.resume()

	waitFor(t, "the auto-invoice is refused and dead-lettered", func() bool {
		return messagingtest.DeadLetterCount(t.Context(), t, f.Pool, orderssubscribers.HandlerAutoInvoice) == 1
	})
	time.Sleep(time.Second) // arch-test:wait-justified — negative proof: the remaining consumers have nothing to signal when they do nothing

	var ord ordersports.OrderDto
	if err := json.Unmarshal(f.authedJSON(t, http.MethodGet, orderPath, tok, nil).body, &ord); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	if ord.State != "packed" || ord.InvoiceID != "" {
		t.Fatalf("order state=%q invoice=%q; want packed with no invoice", ord.State, ord.InvoiceID)
	}
	if r := f.authedJSON(t, http.MethodGet, orderPath+"/invoice", tok, nil); r.status != http.StatusNotFound {
		t.Fatalf("invoice read: %d %s; want 404", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/invoice", tok, nil); r.status != http.StatusConflict || errorCode(t, r.body) != "order_stock_expired" {
		t.Fatalf("manual invoice: %d %s; want 409 order_stock_expired", r.status, r.body)
	}
	if onHand, reserved := stock(); onHand != 20 || reserved != 10 {
		t.Fatalf("stock on hand %v reserved %v; want 20 and 10, untouched", onHand, reserved)
	}
	// Review G2d B1: the pending note the late pack event created cannot leave the building.
	var noteID string
	inCompany(func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT id::text FROM dispatch.consignment_notes WHERE order_id = $1`, orderID).Scan(&noteID)
	})
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes/"+noteID+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-1"}); r.status != http.StatusConflict || errorCode(t, r.body) != "order_not_invoiced" {
		t.Fatalf("dispatch of the pending note: %d %s; want 409 order_not_invoiced", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: "stock expired"}); r.status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
}

// Review G2d B2: the expiry check and the invoice commit are one decision. An invoice that
// has passed the check and then waits on the order's row lock must be judged again when it
// gets the lock: the printed month can end while it waits, and an invoice must not commit
// on expired stock. Here the manual invoice waits behind a held lock on the order row, the
// month ends, the lock is released, and the invoice is refused.
func TestE2E_Orders_AnInvoiceWaitingOnTheOrderLockIsJudgedAfterTheLock(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "invlock")
	tok := reg.AccessToken
	inCompany := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(_ context.Context, tx pgx.Tx) error { return fn(tx) }); err != nil {
			t.Fatalf("company statement: %v", err)
		}
	}
	productID := invCreateProduct(t, f, tok, "INV-LOCK", nil)
	now := time.Now().UTC()
	add := invAddBatch(t, f, tok, productID, "LOT-1", expiry.MonthEnd(now.Year(), now.Month()+6), map[string]any{"initial_quantity": 20})
	if add.status != http.StatusCreated {
		t.Fatalf("add batch: %d %s", add.status, add.body)
	}
	batchID, _ := invObj(t, add.body)["batch_id"].(string)
	stack := startFulfilmentStack(t, f.Pool)
	orderID, orderPath := confirmedOrderOn(t, f, tok, productID, "INV-LOCK")
	waitFor(t, "the confirmed order reserves 10 units", func() bool {
		b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID, tok, nil).body)
		r, _ := b["quantity_reserved"].(float64)
		return r == 10
	})
	stack.pause() // nothing invoices on its own: the manual route is the one under test
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}); r.status != http.StatusNoContent {
		t.Fatalf("pack: %d %s", r.status, r.body)
	}

	// Hold the order row's lock.
	locked := make(chan struct{})
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		defer close(held)
		_ = pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(_ context.Context, tx pgx.Tx) error { // arch-test:ignore-err — a failure shows as the timeout below
			if _, err := tx.Exec(t.Context(), `SELECT 1 FROM orders.orders WHERE id = $1 FOR UPDATE`, orderID); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	// The invoice starts now and waits on the lock.
	invoiced := make(chan httpResp, 1)
	go func() { invoiced <- f.authedJSON(t, http.MethodPost, orderPath+"/invoice", tok, nil) }()
	waitFor(t, "the invoice waits on the order lock", func() bool {
		var n int
		inCompany(func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`).Scan(&n)
		})
		return n > 0
	})

	// The printed month ends while it waits; then the lock is released.
	last := expiry.MonthEnd(now.Year(), now.Month()-1)
	inCompany(func(tx pgx.Tx) error {
		tag, err := tx.Exec(t.Context(), `UPDATE inventory.batches SET expiry_date = $2 WHERE id = $1`, batchID, last)
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("the batch was not aged")
		}
		return err
	})
	close(release)
	<-held

	r := <-invoiced
	if r.status != http.StatusConflict || errorCode(t, r.body) != "order_stock_expired" {
		t.Fatalf("invoice that waited on the lock: %d %s; want 409 order_stock_expired", r.status, r.body)
	}
	var ord ordersports.OrderDto
	if err := json.Unmarshal(f.authedJSON(t, http.MethodGet, orderPath, tok, nil).body, &ord); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	if ord.State != "packed" || ord.InvoiceID != "" {
		t.Fatalf("order state=%q invoice=%q; want packed with no invoice", ord.State, ord.InvoiceID)
	}
}
