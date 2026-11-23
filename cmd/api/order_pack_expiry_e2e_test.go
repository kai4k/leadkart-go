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
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	dispatchadapters "github.com/leadkart/leadkart-go/internal/dispatch/adapters"
	dispatchcommand "github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	dispatchsubscribers "github.com/leadkart/leadkart-go/internal/dispatch/ports/subscribers"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventorycommand "github.com/leadkart/leadkart-go/internal/inventory/app/command"
	inventorysellability "github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	inventorysubscribers "github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
	ordersadapters "github.com/leadkart/leadkart-go/internal/orders/adapters"
	orderscommand "github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
	orderssubscribers "github.com/leadkart/leadkart-go/internal/orders/ports/subscribers"
)

// startFulfilmentStack runs the production event path for an order's fulfilment:
// the outbox forwarder and a router with the subscribers cmd/worker registers for
// reserve, consume, auto-invoice and the consignment note, built the way
// cmd/worker builds them.
func startFulfilmentStack(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	log := silentLogger()
	tx := pg.NewTransactor(pool)
	s := policyStack{
		pool:   pool,
		pubsub: gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(log)),
		tx:     tx,
	}
	t.Cleanup(func() { _ = s.pubsub.Close() })

	// Inventory: reserve on confirm, consume on pack.
	batches := inventoryadapters.NewBatchRepository(pool, tx)
	movements := inventoryadapters.NewStockMovementRepository(pool, tx)
	window := inventorysellability.NewResolver(inventoryadapters.NewPolicyReplicaRepository(pool, tx), time.Now)
	newMID := func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) }
	reserve := inventorysubscribers.NewReserveStockSubscriber(
		inventorycommand.NewReserveOrderStockHandler(tx, batches, movements, movements, inventoryadapters.NewOutboxEnqueuer(), window, time.Now, newMID), log)
	consume := inventorysubscribers.NewConsumeStockSubscriber(
		inventorycommand.NewConsumeOrderStockHandler(tx, batches, movements, movements, window, time.Now, newMID), log)

	// Dispatch: a consignment note slot per packed order.
	dispatchRepo := dispatchadapters.NewConsignmentNoteRepository(pool, tx)
	dispatchPacked := dispatchsubscribers.NewOrderPackedIngestor(
		dispatchcommand.NewCreateConsignmentNoteHandler(tx, dispatchRepo, time.Now,
			func() consignmentnote.ID { return consignmentnote.ID(ids.NewV7().String()) }), log)

	// Orders: auto-invoice on pack, dispatched on the consignment note.
	orderRepo := ordersadapters.NewOrderRepository(pool, tx)
	invoiceOrder := orderscommand.NewInvoiceOrderHandler(tx, orderRepo, ordersadapters.NewInvoiceRepository(pool, tx),
		ordersadapters.NewInvoiceNumberAllocator(pool), time.Now, func() invoice.ID { return invoice.ID(ids.NewV7().String()) })
	attach := orderscommand.NewAttachConsignmentHandler(orderRepo, time.Now)

	var handlers []cqrs.EventHandler
	handlers = append(handlers, inventorysubscribers.Handlers(reserve, consume, nil, nil)...)
	handlers = append(handlers, dispatchsubscribers.Handlers(dispatchPacked, nil)...)
	handlers = append(handlers, orderssubscribers.Handlers(
		orderssubscribers.NewAutoInvoiceSubscriber(invoiceOrder, log),
		orderssubscribers.NewConsignmentCreatedSubscriber(invoiceOrder, attach, log), nil, nil, nil)...)
	s.runRouter(t, handlers)

	forwarder, err := messaging.NewOutboxForwarder(pool, s.pubsub, watermill.NewSlogLogger(log))
	if err != nil {
		t.Fatalf("messaging.NewOutboxForwarder: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		if err := forwarder.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("forwarder: %v", err)
		}
	}()
	t.Cleanup(func() { _ = forwarder.Close() })
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
	q := f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations", tok, ordersports.CreateQuotationRequest{
		CustomerLeadID: uuid.NewString(),
		Items:          []ordersports.LineItemDto{{ProductID: productID, SKU: "PACK-EXP", Quantity: 10, UnitMrpPaise: 10000, UnitSalePaise: 9000, GstRateBps: 1200}},
	})
	if q.status != http.StatusCreated {
		t.Fatalf("create quotation: %d %s", q.status, q.body)
	}
	quoteID, _ := invObj(t, q.body)["quotation_id"].(string)
	ap := f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations/"+quoteID+"/approve", tok, nil)
	if ap.status != http.StatusOK {
		t.Fatalf("approve: %d %s", ap.status, ap.body)
	}
	orderID, _ := invObj(t, ap.body)["order_id"].(string)
	orderPath := "/api/v1/orders/orders/" + orderID
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/token-payment", tok, ordersports.RecordTokenPaymentRequest{
		Method: "upi", AmountPaise: 25000, ExternalReference: "UTR-" + uuid.NewString(),
	}); r.status != http.StatusNoContent {
		t.Fatalf("token payment: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/confirm", tok, nil); r.status != http.StatusNoContent {
		t.Fatalf("confirm: %d %s", r.status, r.body)
	}
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

	// The order is not stuck: it can still be cancelled.
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: "stock expired"}); r.status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
}
