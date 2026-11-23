//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// End-to-end tests for the commerce-integrity fixes (branch
// fix/commerce-integrity): refusals that used to be silent corruption or a
// 500 must now be a stable 422/409 with nothing written.

package main

import (
	"encoding/json"
	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	invports "github.com/leadkart/leadkart-go/internal/inventory/ports"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

// errorCode returns the `error` field of a JSON error body.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Error
}

// ordersApprovedOrder creates + approves a quotation and returns the order path.
func ordersApprovedOrder(t *testing.T, f e2eFixture, tok string) string {
	t.Helper()
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations", tok, ordersports.CreateQuotationRequest{
		CustomerLeadID: uuid.NewString(), Items: ordersSampleItems(),
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create quotation: status %d body %s", resp.status, resp.body)
	}
	var created ordersports.CreateQuotationResponse
	if err := json.Unmarshal(resp.body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp = f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations/"+created.QuotationID+"/approve", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("approve: status %d body %s", resp.status, resp.body)
	}
	var approved ordersports.ApproveQuotationResponse
	if err := json.Unmarshal(resp.body, &approved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return "/api/v1/orders/orders/" + approved.OrderID
}

// ordersTokenPaidOrder returns the path of an order in token_paid.
func ordersTokenPaidOrder(t *testing.T, f e2eFixture, tok string) string {
	t.Helper()
	orderPath := ordersApprovedOrder(t, f, tok)
	resp := f.authedJSON(t, http.MethodPost, orderPath+"/token-payment", tok, ordersports.RecordTokenPaymentRequest{
		Method: "upi", AmountPaise: 25000, ExternalReference: "UTR-" + uuid.NewString(),
	})
	if resp.status != http.StatusNoContent {
		t.Fatalf("token-payment: status %d body %s", resp.status, resp.body)
	}
	return orderPath
}

// ordersConfirmedOrder returns the path of an order in confirmed.
func ordersConfirmedOrder(t *testing.T, f e2eFixture, tok string) string {
	t.Helper()
	orderPath := ordersTokenPaidOrder(t, f, tok)
	resp := f.authedJSON(t, http.MethodPost, orderPath+"/confirm", tok, nil)
	if resp.status != http.StatusNoContent {
		t.Fatalf("confirm: status %d body %s", resp.status, resp.body)
	}
	return orderPath
}

func ordersState(t *testing.T, f e2eFixture, tok, orderPath string) string {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, orderPath, tok, nil)
	var ord ordersports.OrderDto
	if err := json.Unmarshal(resp.body, &ord); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	return ord.State
}

// TestE2E_Orders_PackRefusesWhatDispatchWouldRefuse is the C10 jam: every pack
// the consignment note's rules refuse is a 422 with a stable code and leaves
// the order confirmed; a valid pack still goes through afterwards.
func TestE2E_Orders_PackRefusesWhatDispatchWouldRefuse(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "packjam").AccessToken
	orderPath := ordersConfirmedOrder(t, f, tok)
	past := time.Now().Add(-48 * time.Hour)

	for _, tc := range []struct {
		name string
		body ordersports.PackOrderRequest
		code string
	}{
		{"blank carrier", ordersports.PackOrderRequest{CarrierName: " ", BoxCount: 1, WeightGrams: 100}, "carrier_name_required"},
		{"zero boxes", ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 0, WeightGrams: 100}, "box_count_invalid"},
		{"zero weight", ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 0}, "weight_grams_invalid"},
		{"past delivery", ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 100, ExpectedDeliveryAt: &past}, "expected_delivery_not_in_future"},
	} {
		resp := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, tc.body)
		if resp.status != http.StatusUnprocessableEntity || errorCode(t, resp.body) != tc.code {
			t.Fatalf("%s: status %d body %s; want 422 %s", tc.name, resp.status, resp.body, tc.code)
		}
		if got := ordersState(t, f, tok, orderPath); got != "confirmed" {
			t.Fatalf("%s: order state %s after refused pack, want confirmed", tc.name, got)
		}
	}

	resp := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok,
		ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 100})
	if resp.status != http.StatusNoContent {
		t.Fatalf("valid pack: status %d body %s", resp.status, resp.body)
	}
}

// TestE2E_Inventory_ZeroGSTRateIsKept is C14 over the wire: a product created
// with an explicit gst_rate_bps 0 reads back 0; one created without the field
// takes the category default; an update to 0 sticks.
func TestE2E_Inventory_ZeroGSTRateIsKept(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "gstzero").AccessToken

	create := func(sku string, body map[string]any) string {
		t.Helper()
		body["sku"] = sku
		body["name"] = "Product " + sku
		body["dosage_form"] = "tablet"
		body["pack_size"] = "10x10"
		body["hsn_code"] = "30049099"
		resp := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, body)
		if resp.status != http.StatusCreated {
			t.Fatalf("create %s: status %d body %s", sku, resp.status, resp.body)
		}
		var out invports.CreateProductResponse
		if err := json.Unmarshal(resp.body, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.ProductID
	}
	rate := func(id string) int {
		t.Helper()
		resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products/"+id, tok, nil)
		if resp.status != http.StatusOK {
			t.Fatalf("get product: status %d body %s", resp.status, resp.body)
		}
		var dto invports.ProductDto
		if err := json.Unmarshal(resp.body, &dto); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return dto.GSTRateBps
	}

	zero := create("NIL-0", map[string]any{"gst_rate_bps": 0})
	if got := rate(zero); got != 0 {
		t.Fatalf("explicit 0 stored as %d, want 0", got)
	}
	absent := create("NIL-ABSENT", map[string]any{})
	if got := rate(absent); got == 0 {
		t.Fatalf("absent rate stored as 0, want the category default")
	}
	five := create("NIL-5", map[string]any{"gst_rate_bps": 500})
	resp := f.authedJSON(t, http.MethodPatch, "/api/v1/inventory/products/"+five, tok, map[string]any{"gst_rate_bps": 0})
	if resp.status != http.StatusNoContent && resp.status != http.StatusOK {
		t.Fatalf("update to 0: status %d body %s", resp.status, resp.body)
	}
	if got := rate(five); got != 0 {
		t.Fatalf("updated to 0 but reads %d", got)
	}
}

// invProductWithBatch creates a product and one live batch; returns both ids.
func invProductWithBatch(t *testing.T, f e2eFixture, tok, sku string) (productID, batchID string) {
	t.Helper()
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invports.CreateProductRequest{
		SKU: sku, Name: "Product " + sku, DosageForm: "tablet", PackSize: "10x10", HSNCode: "30049099",
		GSTRateBps: new(1200),
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create product: status %d body %s", resp.status, resp.body)
	}
	var p invports.CreateProductResponse
	if err := json.Unmarshal(resp.body, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp = f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products/"+p.ProductID+"/batches", tok, invports.AddBatchRequest{
		BatchNumber: "B-" + sku, ManufactureDate: time.Now().AddDate(0, -3, 0), ExpiryMonth: expiry.PrintedMonth(time.Now().AddDate(2, 0, 0)),
		ManufacturerName: "Acme", ManufacturingLicenceNumber: "ML-1", MRPPaise: 25000, PurchasePricePaise: 18000,
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("add batch: status %d body %s", resp.status, resp.body)
	}
	var b invports.AddBatchResponse
	if err := json.Unmarshal(resp.body, &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p.ProductID, b.BatchID
}

// TestE2E_Inventory_PublicMovementRouteRefusesReservationAndRelease is I3: a
// stock user must not forge or drop an order's hold through the public route.
func TestE2E_Inventory_PublicMovementRouteRefusesReservationAndRelease(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "movetype").AccessToken
	_, batchID := invProductWithBatch(t, f, tok, "MOVE-1")
	path := "/api/v1/inventory/batches/" + batchID + "/movements"

	for _, typ := range []string{"reservation", "release", "bogus", ""} {
		resp := f.authedJSON(t, http.MethodPost, path, tok, invports.LogMovementRequest{Type: typ, Quantity: 1, Reason: "forged"})
		if resp.status != http.StatusUnprocessableEntity || errorCode(t, resp.body) != "invalid_movement_type" {
			t.Fatalf("type %q: status %d body %s; want 422 invalid_movement_type", typ, resp.status, resp.body)
		}
	}
	// The three public types still work.
	for _, typ := range []string{"inbound", "adjustment", "outbound"} {
		resp := f.authedJSON(t, http.MethodPost, path, tok, invports.LogMovementRequest{Type: typ, Quantity: 1, Reason: "ok"})
		if resp.status != http.StatusCreated {
			t.Fatalf("type %q: status %d body %s; want 201", typ, resp.status, resp.body)
		}
	}
}

// ordersInvoicedOrder returns the path of an order in invoiced (grand total
// 100800: ordersSampleItems).
func ordersInvoicedOrder(t *testing.T, f e2eFixture, tok string) string {
	t.Helper()
	orderPath := ordersConfirmedOrder(t, f, tok)
	resp := f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok,
		ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 100})
	if resp.status != http.StatusNoContent {
		t.Fatalf("pack: status %d body %s", resp.status, resp.body)
	}
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/invoice", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("invoice: status %d body %s", resp.status, resp.body)
	}
	return orderPath
}

func ordersPaymentCount(t *testing.T, f e2eFixture, tok, orderPath string) int {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, orderPath+"/payments", tok, nil)
	var pays ordersports.ListPaymentsResponse
	if err := json.Unmarshal(resp.body, &pays); err != nil {
		t.Fatalf("decode payments: %v", err)
	}
	return len(pays.Payments)
}

// TestE2E_Orders_SecondTokenPaymentIsRefused is C11 (Go part): a repeated
// token payment is 409 and adds no second payment row.
func TestE2E_Orders_SecondTokenPaymentIsRefused(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "tokentwice").AccessToken
	orderPath := ordersTokenPaidOrder(t, f, tok)

	resp := f.authedJSON(t, http.MethodPost, orderPath+"/token-payment", tok, ordersports.RecordTokenPaymentRequest{
		Method: "upi", AmountPaise: 25000, ExternalReference: "UTR-" + uuid.NewString(),
	})
	if resp.status != http.StatusConflict || errorCode(t, resp.body) != "token_payment_already_recorded" {
		t.Fatalf("second token: status %d body %s; want 409 token_payment_already_recorded", resp.status, resp.body)
	}
	if n := ordersPaymentCount(t, f, tok, orderPath); n != 1 {
		t.Fatalf("payments after refused second token = %d, want 1", n)
	}
	if got := ordersState(t, f, tok, orderPath); got != "token_paid" {
		t.Fatalf("order state %s, want token_paid", got)
	}
}

// TestE2E_Orders_PaymentsRouteRefusesToken is C12 (part): kind token on
// POST /payments would write a row without advancing the order.
func TestE2E_Orders_PaymentsRouteRefusesToken(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "paytoken").AccessToken
	orderPath := ordersApprovedOrder(t, f, tok)

	resp := f.authedJSON(t, http.MethodPost, orderPath+"/payments", tok, ordersports.RecordPaymentRequest{
		Kind: "token", Method: "upi", AmountPaise: 25000,
	})
	if resp.status != http.StatusUnprocessableEntity || errorCode(t, resp.body) != "token_payment_route_required" {
		t.Fatalf("token on /payments: status %d body %s; want 422 token_payment_route_required", resp.status, resp.body)
	}
	if n := ordersPaymentCount(t, f, tok, orderPath); n != 0 {
		t.Fatalf("payments after refused token = %d, want 0", n)
	}
	if got := ordersState(t, f, tok, orderPath); got != "quotation_approved" {
		t.Fatalf("order state %s, want quotation_approved", got)
	}
}

// TestE2E_Orders_CreditNotesCappedAtInvoice is C12 (part): the notes on one
// invoice never exceed its total (100800), sequentially and under concurrency.
func TestE2E_Orders_CreditNotesCappedAtInvoice(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "cnnotecap").AccessToken
	orderPath := ordersInvoicedOrder(t, f, tok)
	mint := func(amount int64) (int, []byte) {
		resp := f.authedJSON(t, http.MethodPost, orderPath+"/credit-notes", tok,
			ordersports.MintCreditNoteRequest{Reason: "return", AmountPaise: amount})
		return resp.status, resp.body
	}

	if st, body := mint(100801); st != http.StatusUnprocessableEntity || errorCode(t, body) != "credit_note_exceeds_invoice" {
		t.Fatalf("over total: status %d body %s; want 422 credit_note_exceeds_invoice", st, body)
	}
	if st, body := mint(100000); st != http.StatusCreated {
		t.Fatalf("100000: status %d body %s", st, body)
	}
	if st, body := mint(801); st != http.StatusUnprocessableEntity || errorCode(t, body) != "credit_note_exceeds_invoice" {
		t.Fatalf("801 after 100000: status %d body %s; want 422", st, body)
	}
	if st, body := mint(800); st != http.StatusCreated {
		t.Fatalf("800 (the exact remainder): status %d body %s", st, body)
	}
	if st, body := mint(1); st != http.StatusUnprocessableEntity || errorCode(t, body) != "credit_note_exceeds_invoice" {
		t.Fatalf("1 after the invoice is fully credited: status %d body %s; want 422", st, body)
	}
}

// TestE2E_Orders_ConcurrentCreditNotesCannotExceedInvoice fires many notes at
// once on a fresh invoice: the invoice row lock must let exactly the notes
// that fit through (5 x 20000 <= 100800 < 6 x 20000).
func TestE2E_Orders_ConcurrentCreditNotesCannotExceedInvoice(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "cnnoterace").AccessToken
	orderPath := ordersInvoicedOrder(t, f, tok)

	const workers = 10
	statuses := make(chan int, workers)
	for range workers {
		go func() {
			resp := f.authedJSON(t, http.MethodPost, orderPath+"/credit-notes", tok,
				ordersports.MintCreditNoteRequest{Reason: "race", AmountPaise: 20000})
			statuses <- resp.status
		}()
	}
	created := 0
	for range workers {
		switch st := <-statuses; st {
		case http.StatusCreated:
			created++
		case http.StatusUnprocessableEntity:
		default:
			t.Fatalf("unexpected status %d from a concurrent credit note", st)
		}
	}
	if created != 5 {
		t.Fatalf("created %d notes of 20000 on a 100800 invoice, want exactly 5", created)
	}
}

// TestE2E_Orders_TextOverItsDatabaseLimitIs422 is I6: a payment reference over
// 100, payment notes over 2000, a cancel reason over 1000 and a credit-note
// reason over 1000 characters were database CHECK violations (500). Each is a
// field-named 422 now; the value at the limit still goes through.
func TestE2E_Orders_TextOverItsDatabaseLimitIs422(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "ordertext").AccessToken
	want := func(stage string, status int, body []byte, code string) {
		t.Helper()
		if status != http.StatusUnprocessableEntity || errorCode(t, body) != code {
			t.Fatalf("%s: status %d body %s; want 422 %s", stage, status, body, code)
		}
	}

	// Payment fields (token payment and the general payments route).
	orderPath := ordersApprovedOrder(t, f, tok)
	resp := f.authedJSON(t, http.MethodPost, orderPath+"/token-payment", tok, ordersports.RecordTokenPaymentRequest{
		Method: "upi", AmountPaise: 25000, ExternalReference: strings.Repeat("r", 101),
	})
	want("token reference 101", resp.status, resp.body, "external_reference_too_long")
	if got := ordersState(t, f, tok, orderPath); got != "quotation_approved" {
		t.Fatalf("refused token payment moved the order to %s", got)
	}
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/token-payment", tok, ordersports.RecordTokenPaymentRequest{
		Method: "upi", AmountPaise: 25000, ExternalReference: strings.Repeat("é", 100), Notes: strings.Repeat("n", 2000),
	})
	if resp.status != http.StatusNoContent {
		t.Fatalf("token payment at the limits: status %d body %s; want 204", resp.status, resp.body)
	}
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/payments", tok, ordersports.RecordPaymentRequest{
		Kind: "full", Method: "upi", AmountPaise: 100, ExternalReference: strings.Repeat("r", 101),
	})
	want("payment reference 101", resp.status, resp.body, "external_reference_too_long")
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/payments", tok, ordersports.RecordPaymentRequest{
		Kind: "full", Method: "upi", AmountPaise: 100, Notes: strings.Repeat("n", 2001),
	})
	want("payment notes 2001", resp.status, resp.body, "notes_too_long")

	// Cancel reason.
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: strings.Repeat("c", 1001)})
	want("cancel reason 1001", resp.status, resp.body, "reason_too_long")
	if got := ordersState(t, f, tok, orderPath); got == "cancelled" {
		t.Fatal("refused cancel changed the order")
	}
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: strings.Repeat("é", 1000)})
	if resp.status != http.StatusNoContent {
		t.Fatalf("cancel reason at 1000: status %d body %s; want 204", resp.status, resp.body)
	}

	// Credit-note reason.
	invoiced := ordersInvoicedOrder(t, f, tok)
	resp = f.authedJSON(t, http.MethodPost, invoiced+"/credit-notes", tok,
		ordersports.MintCreditNoteRequest{Reason: strings.Repeat("c", 1001), AmountPaise: 100})
	want("credit note reason 1001", resp.status, resp.body, "reason_too_long")
}

// TestE2E_Dispatch_TextOverItsDatabaseLimitIs422: docket number over 100 and
// failure reason over 1000 characters, plus a carrier over 200.
func TestE2E_Dispatch_TextOverItsDatabaseLimitIs422(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "dispatchtext").AccessToken
	want := func(stage string, status int, body []byte, code string) {
		t.Helper()
		if status != http.StatusUnprocessableEntity || errorCode(t, body) != code {
			t.Fatalf("%s: status %d body %s; want 422 %s", stage, status, body, code)
		}
	}
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
		OrderID: uuid.NewString(), CarrierName: strings.Repeat("c", 201), BoxCount: 1, WeightGrams: 100,
	})
	want("carrier 201", resp.status, resp.body, "carrier_name_too_long")

	create := func() string {
		resp := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
			OrderID: uuid.NewString(), CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 100,
		})
		var out dispatchports.CreateConsignmentNoteResponse
		if err := json.Unmarshal(resp.body, &out); err != nil || resp.status != http.StatusCreated {
			t.Fatalf("create note: status %d body %s", resp.status, resp.body)
		}
		return "/api/v1/dispatch/consignment-notes/" + out.ConsignmentNoteID
	}
	base := create()
	resp = f.authedJSON(t, http.MethodPost, base+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: strings.Repeat("d", 101)})
	want("docket 101", resp.status, resp.body, "docket_number_too_long")
	resp = f.authedJSON(t, http.MethodPost, base+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: strings.Repeat("é", 100)})
	if resp.status != http.StatusNoContent {
		t.Fatalf("docket at 100: status %d body %s; want 204", resp.status, resp.body)
	}

	base = create()
	resp = f.authedJSON(t, http.MethodPost, base+"/failed", tok, dispatchports.MarkFailedRequest{Reason: strings.Repeat("f", 1001)})
	want("failure reason 1001", resp.status, resp.body, "reason_too_long")
	resp = f.authedJSON(t, http.MethodPost, base+"/failed", tok, dispatchports.MarkFailedRequest{Reason: strings.Repeat("é", 1000)})
	if resp.status != http.StatusNoContent {
		t.Fatalf("failure reason at 1000: status %d body %s; want 204", resp.status, resp.body)
	}
}

// TestE2E_Inventory_HSNRule is I7 over the wire: 0402 (a real chapter 04 code)
// is accepted; 5, 7, 9 and 10 digits are 422 (the old Product rule took them).
func TestE2E_Inventory_HSNRule(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "hsnrule").AccessToken
	create := func(sku, hsn string) (int, []byte) {
		resp := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invports.CreateProductRequest{
			SKU: sku, Name: "Product " + sku, DosageForm: "tablet", PackSize: "10x10", HSNCode: hsn, GSTRateBps: new(500),
		})
		return resp.status, resp.body
	}
	if st, body := create("HSN-0402", "0402"); st != http.StatusCreated {
		t.Fatalf("hsn 0402: status %d body %s; want 201", st, body)
	}
	if st, body := create("HSN-SP", " 30049099 "); st != http.StatusCreated {
		t.Fatalf("padded hsn: status %d body %s; want 201 (trimmed)", st, body)
	}
	for i, hsn := range []string{"30041", "3004102", "300410200", "3004102000", "300", "3004XX"} {
		if st, body := create("HSN-BAD-"+string(rune('A'+i)), hsn); st != http.StatusUnprocessableEntity {
			t.Fatalf("hsn %q: status %d body %s; want 422", hsn, st, body)
		}
	}
}
