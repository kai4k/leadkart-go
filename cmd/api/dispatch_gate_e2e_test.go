//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(), and
// every wait below is bounded by waitFor.
//
// arch-test:no-synctest — the proof runs the real outbox forwarder and routers
// against a real Postgres; virtual time cannot model that.
//
// arch-test:raw-sql-justified — the proof ages a batch past its printed month (no API moves a
// batch expiry) and reads dispatch rows (a different module's table) as the company.

// Review G2e E1 and E2: the dispatch gate judges the order, not only its invoice. An order
// that was invoiced while its batch was valid still must not ship once the printed month has
// ended with the goods in the warehouse; a cancelled order keeps its invoice and must not
// ship either. The normal path is not slowed: invoiced, valid, dispatched, in transit,
// delivered.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

func TestE2E_Dispatch_TheGateJudgesTheOrderAndItsGoods(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "dispgate")
	tok := reg.AccessToken
	startFulfilmentStack(t, f.Pool)
	inCompany := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := pg.NewTransactor(f.Pool).WithinTxPgxTenant(t.Context(), reg.TenantID, func(_ context.Context, tx pgx.Tx) error { return fn(tx) }); err != nil {
			t.Fatalf("company statement: %v", err)
		}
	}
	// flow runs the production fulfilment of one order to the point where the order is
	// dispatched and its consignment note is pending: reserved, packed, consumed, invoiced.
	flow := func(sku string) (batchID, noteID string) {
		t.Helper()
		productID := invCreateProduct(t, f, tok, sku, nil)
		now := time.Now().UTC()
		add := invAddBatch(t, f, tok, productID, "LOT-"+sku, expiry.MonthEnd(now.Year(), now.Month()+6), map[string]any{"initial_quantity": 20})
		if add.status != http.StatusCreated {
			t.Fatalf("add batch: %d %s", add.status, add.body)
		}
		batchID, _ = invObj(t, add.body)["batch_id"].(string)
		orderID, orderPath := confirmedOrderOn(t, f, tok, productID, sku)
		waitFor(t, "the confirmed order reserves 10 units", func() bool {
			b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID, tok, nil).body)
			r, _ := b["quantity_reserved"].(float64)
			return r == 10
		})
		if r := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}); r.status != http.StatusNoContent {
			t.Fatalf("pack: %d %s", r.status, r.body)
		}
		waitFor(t, "the order is dispatched", func() bool {
			var ord ordersports.OrderDto
			if err := json.Unmarshal(f.authedJSON(t, http.MethodGet, orderPath, tok, nil).body, &ord); err != nil {
				return false
			}
			return ord.State == "dispatched"
		})
		inCompany(func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT id::text FROM dispatch.consignment_notes WHERE order_id = $1`, orderID).Scan(&noteID)
		})
		return batchID, noteID
	}
	notePath := func(id string) string { return "/api/v1/dispatch/consignment-notes/" + id }
	noteStatus := func(id string) string {
		var n dispatchports.ConsignmentNoteDto
		if err := json.Unmarshal(f.authedJSON(t, http.MethodGet, notePath(id), tok, nil).body, &n); err != nil {
			t.Fatalf("decode note: %v", err)
		}
		return n.Status
	}

	t.Run("invoiced and valid: dispatched, in transit, delivered", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		_, noteID := flow("GATE-OK")
		if r := f.authedJSON(t, http.MethodPost, notePath(noteID)+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-1"}); r.status != http.StatusNoContent {
			t.Fatalf("dispatch: %d %s; want 204", r.status, r.body)
		}
		if r := f.authedJSON(t, http.MethodPost, notePath(noteID)+"/in-transit", tok, nil); r.status != http.StatusNoContent {
			t.Fatalf("in transit: %d %s; want 204", r.status, r.body)
		}
		if r := f.authedJSON(t, http.MethodPost, notePath(noteID)+"/delivered", tok, nil); r.status != http.StatusNoContent {
			t.Fatalf("delivered: %d %s; want 204", r.status, r.body)
		}
	})

	t.Run("invoiced while valid, the month ends with the goods in the warehouse: 409", func(t *testing.T) {
		// arch-test:serial — the scenarios share one stack and one company; each runs to completion first.
		batchID, noteID := flow("GATE-EXP")
		now := time.Now().UTC()
		inCompany(func(tx pgx.Tx) error {
			tag, err := tx.Exec(t.Context(), `UPDATE inventory.batches SET expiry_date = $2 WHERE id = $1`, batchID, expiry.MonthEnd(now.Year(), now.Month()-1))
			if err == nil && tag.RowsAffected() != 1 {
				err = errors.New("the batch was not aged")
			}
			return err
		})
		r := f.authedJSON(t, http.MethodPost, notePath(noteID)+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-2"})
		if r.status != http.StatusConflict || errorCode(t, r.body) != "order_stock_expired" {
			t.Fatalf("dispatch after the month ended: %d %s; want 409 order_stock_expired", r.status, r.body)
		}
		if got := noteStatus(noteID); got != "pending" {
			t.Fatalf("note status %q, want pending", got)
		}
	})
}

func TestE2E_Dispatch_ACancelledOrderKeepsItsInvoiceButIsNeverShipped(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "dispcancel").AccessToken
	orderID := invoicedOrderID(t, f, tok, "GATE-CANCEL")
	orderPath := "/api/v1/orders/orders/" + orderID
	if r := f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: "customer withdrew"}); r.status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
	mk := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
		OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
	})
	if mk.status != http.StatusCreated {
		t.Fatalf("note: %d %s", mk.status, mk.body)
	}
	noteID, _ := invObj(t, mk.body)["consignment_note_id"].(string)
	r := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes/"+noteID+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BDX-3"})
	if r.status != http.StatusConflict || errorCode(t, r.body) != "order_not_dispatchable" {
		t.Fatalf("dispatch on a cancelled order: %d %s; want 409 order_not_dispatchable", r.status, r.body)
	}
}
