//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// End-to-end tests for the fix round after the commerce-integrity review
// (branch fix/commerce-integrity; .superpowers/sdd/review-commerce-integrity.md).

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

// nul is a value holding U+0000: valid JSON ("\u0000"), impossible to store in a
// PostgreSQL text or jsonb value.
const nul = "bad\x00value"

// TestE2E_Commerce_NULInTextIs422NeverA500 is M1 (and I2 over the wire): every
// free-text input of the orders, dispatch and inventory write routes refuses a
// value holding U+0000 with a 422 and a code that names the field, before any
// write. Each used to reach the insert and answer 500 (SQLSTATE 22021 / 22P05);
// a NUL in the pack's carrier name was worse: 204, order packed, and the
// consignment note then failed on every delivery of the event.
func TestE2E_Commerce_NULInTextIs422NeverA500(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "nultext").AccessToken
	want := func(stage string, r httpResp, code string) {
		t.Helper()
		if r.status != http.StatusUnprocessableEntity || errorCode(t, r.body) != code {
			t.Errorf("%s: status %d body %s; want 422 %s", stage, r.status, r.body, code)
		}
		if strings.Contains(string(r.body), "bad") {
			t.Errorf("%s: the refusal echoes the value: %s", stage, r.body)
		}
	}
	post := func(path string, body any) httpResp { return f.authedJSON(t, http.MethodPost, path, tok, body) }

	// ----- orders: quotations -------------------------------------------------
	item := func(edit func(*ordersports.LineItemDto)) []ordersports.LineItemDto {
		items := ordersSampleItems()
		edit(&items[0])
		return items
	}
	quotes := "/api/v1/orders/quotations"
	want("quotation note", post(quotes, ordersports.CreateQuotationRequest{CustomerLeadID: uuid.NewString(), Items: ordersSampleItems(), Note: nul}), "note_invalid_characters")
	want("quotation item sku", post(quotes, ordersports.CreateQuotationRequest{CustomerLeadID: uuid.NewString(),
		Items: item(func(i *ordersports.LineItemDto) { i.SKU = nul })}), "sku_invalid_characters")
	want("quotation item description", post(quotes, ordersports.CreateQuotationRequest{CustomerLeadID: uuid.NewString(),
		Items: item(func(i *ordersports.LineItemDto) { i.Description = nul })}), "description_invalid_characters")
	// An id is not free text: it was already refused as a malformed UUID.
	if r := post(quotes, ordersports.CreateQuotationRequest{CustomerLeadID: uuid.NewString(),
		Items: item(func(i *ordersports.LineItemDto) { i.ProductID = nul })}); r.status != http.StatusBadRequest || errorCode(t, r.body) != "invalid_body" {
		t.Errorf("quotation item product id: status %d body %s; want 400 invalid_body", r.status, r.body)
	}

	resp := post(quotes, ordersports.CreateQuotationRequest{CustomerLeadID: uuid.NewString(), Items: ordersSampleItems()})
	var q ordersports.CreateQuotationResponse
	if err := json.Unmarshal(resp.body, &q); err != nil || resp.status != http.StatusCreated {
		t.Fatalf("create quotation: status %d body %s", resp.status, resp.body)
	}
	want("revise note", post(quotes+"/"+q.QuotationID+"/revise", ordersports.ReviseQuotationRequest{Items: ordersSampleItems(), Note: nul}), "note_invalid_characters")
	want("revise item description", post(quotes+"/"+q.QuotationID+"/revise", ordersports.ReviseQuotationRequest{
		Items: item(func(i *ordersports.LineItemDto) { i.Description = nul })}), "description_invalid_characters")
	want("reject reason", post(quotes+"/"+q.QuotationID+"/reject", ordersports.RejectQuotationRequest{Reason: nul}), "reason_invalid_characters")

	// ----- orders: payments, cancel, pack, credit note --------------------------
	orderPath := ordersApprovedOrder(t, f, tok)
	want("token reference", post(orderPath+"/token-payment", ordersports.RecordTokenPaymentRequest{Method: "upi", AmountPaise: 25000, ExternalReference: nul}), "external_reference_invalid_characters")
	want("token notes", post(orderPath+"/token-payment", ordersports.RecordTokenPaymentRequest{Method: "upi", AmountPaise: 25000, Notes: nul}), "notes_invalid_characters")
	if got := ordersState(t, f, tok, orderPath); got != "quotation_approved" {
		t.Fatalf("a refused token payment moved the order to %s", got)
	}
	want("payment reference", post(orderPath+"/payments", ordersports.RecordPaymentRequest{Kind: "full", Method: "upi", AmountPaise: 100, ExternalReference: nul}), "external_reference_invalid_characters")
	want("payment notes", post(orderPath+"/payments", ordersports.RecordPaymentRequest{Kind: "full", Method: "upi", AmountPaise: 100, Notes: nul}), "notes_invalid_characters")
	if n := ordersPaymentCount(t, f, tok, orderPath); n != 0 {
		t.Fatalf("a refused payment was recorded: %d rows", n)
	}
	want("cancel reason", post(orderPath+"/cancel", ordersports.CancelOrderRequest{Reason: nul}), "reason_invalid_characters")
	if got := ordersState(t, f, tok, orderPath); got != "quotation_approved" {
		t.Fatalf("a refused cancel moved the order to %s", got)
	}

	confirmed := ordersConfirmedOrder(t, f, tok)
	want("pack carrier", post(confirmed+"/pack", ordersports.PackOrderRequest{CarrierName: "Blue\x00Dart", BoxCount: 1, WeightGrams: 100}), "carrier_name_invalid_characters")
	if got := ordersState(t, f, tok, confirmed); got != "confirmed" {
		t.Fatalf("a refused pack moved the order to %s (the saga would jam on the consignment note)", got)
	}

	invoiced := ordersInvoicedOrder(t, f, tok)
	want("credit note reason", post(invoiced+"/credit-notes", ordersports.MintCreditNoteRequest{Reason: nul, AmountPaise: 100}), "reason_invalid_characters")

	// ----- dispatch -------------------------------------------------------------
	notes := "/api/v1/dispatch/consignment-notes"
	want("consignment carrier", post(notes, dispatchports.CreateConsignmentNoteRequest{OrderID: uuid.NewString(), CarrierName: "Blue\x00Dart", BoxCount: 1, WeightGrams: 100}), "carrier_name_invalid_characters")
	newNote := func() string {
		r := post(notes, dispatchports.CreateConsignmentNoteRequest{OrderID: uuid.NewString(), CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 100})
		var out dispatchports.CreateConsignmentNoteResponse
		if err := json.Unmarshal(r.body, &out); err != nil || r.status != http.StatusCreated {
			t.Fatalf("create note: status %d body %s", r.status, r.body)
		}
		return notes + "/" + out.ConsignmentNoteID
	}
	base := newNote()
	want("docket number", post(base+"/dispatch", dispatchports.MarkDispatchedRequest{DocketNumber: nul}), "docket_number_invalid_characters")
	want("failure reason", post(base+"/failed", dispatchports.MarkFailedRequest{Reason: nul}), "reason_invalid_characters")

	// ----- inventory (its refusals keep the one code it has always used) ---------
	products := "/api/v1/inventory/products"
	product := func(edit func(*invports.CreateProductRequest)) invports.CreateProductRequest {
		p := invports.CreateProductRequest{SKU: "NUL-" + uuid.NewString()[:8], Name: "Product", DosageForm: "tablet", PackSize: "10x10", HSNCode: "30049099", GSTRateBps: new(1200)}
		edit(&p)
		return p
	}
	want("product sku", post(products, product(func(p *invports.CreateProductRequest) { p.SKU = nul })), "validation_failed")
	want("product name", post(products, product(func(p *invports.CreateProductRequest) { p.Name = nul })), "validation_failed")
	want("product dosage form", post(products, product(func(p *invports.CreateProductRequest) { p.DosageForm = nul })), "validation_failed")
	want("product pack size", post(products, product(func(p *invports.CreateProductRequest) { p.PackSize = nul })), "validation_failed")
	want("product manufacturer", post(products, product(func(p *invports.CreateProductRequest) { p.Manufacturer = nul })), "validation_failed")

	productID, batchID := invProductWithBatch(t, f, tok, "NULOK-1")
	bad := nul
	want("update name", f.authedJSON(t, http.MethodPatch, products+"/"+productID, tok, invports.UpdateProductRequest{Name: &bad}), "validation_failed")
	want("update manufacturer", f.authedJSON(t, http.MethodPatch, products+"/"+productID, tok, invports.UpdateProductRequest{Manufacturer: &bad}), "validation_failed")

	batch := func(edit func(*invports.AddBatchRequest)) invports.AddBatchRequest {
		b := invports.AddBatchRequest{BatchNumber: "B-" + uuid.NewString()[:8], ManufactureDate: time.Now().AddDate(0, -3, 0), ExpiryMonth: expiry.PrintedMonth(time.Now().AddDate(2, 0, 0)),
			ManufacturerName: "Acme", ManufacturingLicenceNumber: "ML-1", MRPPaise: 25000, PurchasePricePaise: 18000}
		edit(&b)
		return b
	}
	batches := products + "/" + productID + "/batches"
	want("batch number", post(batches, batch(func(b *invports.AddBatchRequest) { b.BatchNumber = nul })), "validation_failed")
	want("batch manufacturer", post(batches, batch(func(b *invports.AddBatchRequest) { b.ManufacturerName = nul })), "validation_failed")
	want("batch licence", post(batches, batch(func(b *invports.AddBatchRequest) { b.ManufacturingLicenceNumber = nul })), "validation_failed")

	moves := "/api/v1/inventory/batches/" + batchID + "/movements"
	want("movement reason", post(moves, invports.LogMovementRequest{Type: "inbound", Quantity: 1, Reason: nul}), "validation_failed")
	want("movement source reference", post(moves, invports.LogMovementRequest{Type: "inbound", Quantity: 1, Reason: "ok", SourceReference: &bad}), "validation_failed")

	// The list filters are text too: %00 in a filter is a 400, not a 500.
	for _, param := range []string{"search", "dosage_form", "manufacturer"} {
		r := f.authedJSON(t, http.MethodGet, products+"?"+param+"=a%00b", tok, nil)
		if r.status != http.StatusBadRequest || errorCode(t, r.body) != "invalid_query" {
			t.Errorf("list filter %s with NUL: status %d body %s; want 400 invalid_query", param, r.status, r.body)
		}
	}

	// Multi-line notes stay valid: only U+0000 is refused in free text.
	if r := post(orderPath+"/payments", ordersports.RecordPaymentRequest{Kind: "full", Method: "upi", AmountPaise: 100, Notes: "line one\nline two\tcash"}); r.status != http.StatusCreated {
		t.Errorf("multi-line payment notes: status %d body %s; want 201", r.status, r.body)
	}
}

// TestE2E_Idempotency_A204CommandIsReplayed is I4, through the real public chain
// and the Postgres store. A 204 has no body; the store's response_body column is
// NOT NULL and the failed write was discarded, so NO 204 command was ever stored:
// a token payment that committed but whose answer was lost, retried with the same
// X-Command-Id, answered 409 token_payment_already_recorded (a false failure).
// The retry must answer what the first call answered: 204, marked as a replay,
// with one payment row. The same holds for every other 204 command.
func TestE2E_Idempotency_A204CommandIsReplayed(t *testing.T) {
	f, _ := newE2EChainFixture(t, nil)
	tok := f.registerAndLogin(t, "idem204").AccessToken
	orderPath := ordersApprovedOrder(t, f, tok)

	send := func(path string, body any, commandID string) httpResp {
		t.Helper()
		return f.doWithHeaders(t, http.MethodPost, path, tok, body,
			map[string]string{"X-Forwarded-For": "198.51.100.77", "X-Command-Id": commandID})
	}
	retried := func(stage, path string, body any) {
		t.Helper()
		id := uuid.NewString()
		first := send(path, body, id)
		if first.status != http.StatusNoContent || first.header.Get("X-Idempotent-Replay") != "" {
			t.Fatalf("%s: first call status %d replay=%q body %s; want a fresh 204", stage, first.status, first.header.Get("X-Idempotent-Replay"), first.body)
		}
		again := send(path, body, id)
		if again.status != http.StatusNoContent || again.header.Get("X-Idempotent-Replay") != "true" || len(again.body) != 0 {
			t.Fatalf("%s: retry with the same X-Command-Id: status %d replay=%q body %s; want 204 with X-Idempotent-Replay: true",
				stage, again.status, again.header.Get("X-Idempotent-Replay"), again.body)
		}
	}

	token := ordersports.RecordTokenPaymentRequest{Method: "upi", AmountPaise: 25000, ExternalReference: "UTR-" + uuid.NewString()}
	retried("token payment", orderPath+"/token-payment", token)
	if n := ordersPaymentCount(t, f, tok, orderPath); n != 1 {
		t.Fatalf("payments after a retried token payment: %d, want 1", n)
	}
	if got := ordersState(t, f, tok, orderPath); got != "token_paid" {
		t.Fatalf("order state %s, want token_paid", got)
	}
	// A DIFFERENT command id is a new command: the guard still refuses it.
	if r := send(orderPath+"/token-payment", token, uuid.NewString()); r.status != http.StatusConflict {
		t.Fatalf("second token payment under a new command id: status %d body %s; want 409", r.status, r.body)
	}

	// Every other 204 command is replayed the same way.
	retried("confirm", orderPath+"/confirm", nil)
	retried("pack", orderPath+"/pack", ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 100})
	retried("cancel", orderPath+"/cancel", ordersports.CancelOrderRequest{Reason: "customer withdrew"})
}

// TestE2E_Inventory_LimitsCountCharactersNotBytes is M2 over the wire and
// against the real column CHECKs (length() counts characters): a Devanagari
// value AT each limit is accepted and stored (it used to be refused: the domain
// counted bytes, 3 per letter), and one character more is a 422, not a 500.
func TestE2E_Inventory_LimitsCountCharactersNotBytes(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "hindilimits").AccessToken
	dev := func(n int) string { return strings.Repeat("क", n) }
	products := "/api/v1/inventory/products"
	refused := func(stage string, r httpResp) {
		t.Helper()
		if r.status != http.StatusUnprocessableEntity || errorCode(t, r.body) != "validation_failed" {
			t.Errorf("%s: status %d body %s; want 422 validation_failed", stage, r.status, r.body)
		}
	}

	// Product: every text field at its limit, in Devanagari.
	atLimit := invports.CreateProductRequest{
		SKU: dev(64), Name: dev(200), DosageForm: dev(50), PackSize: dev(100), HSNCode: "30049099",
		GSTRateBps: new(1200), Manufacturer: dev(200),
	}
	resp := f.authedJSON(t, http.MethodPost, products, tok, atLimit)
	var p invports.CreateProductResponse
	if err := json.Unmarshal(resp.body, &p); err != nil || resp.status != http.StatusCreated {
		t.Fatalf("product with every field at its limit in Devanagari: status %d body %s; want 201", resp.status, resp.body)
	}
	for field, edit := range map[string]func(*invports.CreateProductRequest){
		"sku":          func(r *invports.CreateProductRequest) { r.SKU = dev(65) },
		"name":         func(r *invports.CreateProductRequest) { r.Name = dev(201) },
		"dosage_form":  func(r *invports.CreateProductRequest) { r.DosageForm = dev(51) },
		"pack_size":    func(r *invports.CreateProductRequest) { r.PackSize = dev(101) },
		"manufacturer": func(r *invports.CreateProductRequest) { r.Manufacturer = dev(201) },
	} {
		over := atLimit
		over.SKU = "OVER-" + uuid.NewString()[:8]
		edit(&over)
		refused("product "+field+" one over", f.authedJSON(t, http.MethodPost, products, tok, over))
	}

	// Batch: number, manufacturer and licence at their limits.
	batches := products + "/" + p.ProductID + "/batches"
	batch := invports.AddBatchRequest{
		BatchNumber: dev(100), ManufactureDate: time.Now().AddDate(0, -3, 0), ExpiryMonth: expiry.PrintedMonth(time.Now().AddDate(2, 0, 0)),
		ManufacturerName: dev(200), ManufacturingLicenceNumber: dev(100), MRPPaise: 25000, PurchasePricePaise: 18000,
	}
	resp = f.authedJSON(t, http.MethodPost, batches, tok, batch)
	var b invports.AddBatchResponse
	if err := json.Unmarshal(resp.body, &b); err != nil || resp.status != http.StatusCreated {
		t.Fatalf("batch with every field at its limit in Devanagari: status %d body %s; want 201", resp.status, resp.body)
	}
	over := batch
	over.BatchNumber = dev(101)
	refused("batch number one over", f.authedJSON(t, http.MethodPost, batches, tok, over))

	// Movement: reason 500 and source reference 200.
	moves := "/api/v1/inventory/batches/" + b.BatchID + "/movements"
	ref := dev(200)
	resp = f.authedJSON(t, http.MethodPost, moves, tok, invports.LogMovementRequest{Type: "inbound", Quantity: 5, Reason: dev(500), SourceReference: &ref})
	if resp.status != http.StatusCreated {
		t.Fatalf("movement with a 500-character Devanagari reason: status %d body %s; want 201", resp.status, resp.body)
	}
	refused("movement reason one over", f.authedJSON(t, http.MethodPost, moves, tok, invports.LogMovementRequest{Type: "inbound", Quantity: 5, Reason: dev(501)}))
	longRef := dev(201)
	refused("movement source reference one over", f.authedJSON(t, http.MethodPost, moves, tok,
		invports.LogMovementRequest{Type: "inbound", Quantity: 5, Reason: "ok", SourceReference: &longRef}))
}
