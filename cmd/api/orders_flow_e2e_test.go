//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// End-to-end HTTP contract test for the Orders + Dispatch surfaces: drives the
// quote→order fulfillment lifecycle through the REAL ServeMux (authz gates,
// permission grants, JSON wire shapes, error mapping) against testcontainers
// Postgres. The saga subscribers live in cmd/worker, so the in-process leg
// uses the manual invoice route; cross-module event delivery is covered by
// the worker-side subscriber tests.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

func ordersSampleItems() []ordersports.LineItemDto {
	return []ordersports.LineItemDto{{
		ProductID:     uuid.NewString(),
		SKU:           "SKU-PCM-500",
		Description:   "Paracetamol 500mg",
		Quantity:      10,
		UnitMrpPaise:  10000,
		UnitSalePaise: 9000,
		GstRateBps:    1200,
	}}
}

// TestE2E_Orders_QuoteToInvoiceLifecycle drives the full operator path:
// quotation create → read → approve(+order) → token-payment → confirm → pack →
// invoice (gapless number) → invoice read → payments list, asserting status
// codes, wire shapes, and the money math at each hop.
func TestE2E_Orders_QuoteToInvoiceLifecycle(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "ordersflow")
	tok := owner.AccessToken

	// Create quotation.
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations", tok, ordersports.CreateQuotationRequest{
		CustomerLeadID: uuid.NewString(),
		Items:          ordersSampleItems(),
		Note:           "first quote",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create quotation: status %d body %s", resp.status, resp.body)
	}
	var created ordersports.CreateQuotationResponse
	if err := json.Unmarshal(resp.body, &created); err != nil {
		t.Fatalf("create quotation decode: %v", err)
	}

	// Read it back.
	resp = f.authedJSON(t, http.MethodGet, "/api/v1/orders/quotations/"+created.QuotationID, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get quotation: status %d body %s", resp.status, resp.body)
	}
	var quote ordersports.QuotationDto
	if err := json.Unmarshal(resp.body, &quote); err != nil {
		t.Fatalf("get quotation decode: %v", err)
	}
	if quote.State != "draft" || quote.RevisionNumber != 1 {
		t.Fatalf("quotation state=%s rev=%d want draft rev 1", quote.State, quote.RevisionNumber)
	}

	// Approve → seeds the order.
	resp = f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations/"+created.QuotationID+"/approve", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("approve: status %d body %s", resp.status, resp.body)
	}
	var approved ordersports.ApproveQuotationResponse
	if err := json.Unmarshal(resp.body, &approved); err != nil {
		t.Fatalf("approve decode: %v", err)
	}
	orderPath := "/api/v1/orders/orders/" + approved.OrderID

	// Order read: quotation_approved with computed totals (90000 + 12% GST).
	var ord ordersports.OrderDto
	mustGet := func(stage string) {
		t.Helper()
		resp = f.authedJSON(t, http.MethodGet, orderPath, tok, nil)
		if resp.status != http.StatusOK {
			t.Fatalf("%s get order: status %d body %s", stage, resp.status, resp.body)
		}
		if err := json.Unmarshal(resp.body, &ord); err != nil {
			t.Fatalf("%s get order decode: %v", stage, err)
		}
	}
	mustGet("post-approve")
	if ord.State != "quotation_approved" || ord.GrandTotalPaise != 100800 {
		t.Fatalf("order state=%s total=%d want quotation_approved 100800", ord.State, ord.GrandTotalPaise)
	}

	// Token payment (payments.record gate) → token_paid.
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/token-payment", tok, ordersports.RecordTokenPaymentRequest{
		Method: "upi", AmountPaise: 25000, ExternalReference: "UTR-" + uuid.NewString(),
	})
	if resp.status != http.StatusNoContent {
		t.Fatalf("token-payment: status %d body %s", resp.status, resp.body)
	}

	// Confirm → pack → manual invoice.
	for _, step := range []string{"/confirm", "/pack"} {
		var body any
		if step == "/pack" {
			body = ordersports.PackOrderRequest{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500}
		}
		resp = f.authedJSON(t, http.MethodPost, orderPath+step, tok, body)
		if resp.status != http.StatusNoContent {
			t.Fatalf("%s: status %d body %s", step, resp.status, resp.body)
		}
	}
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/invoice", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("invoice: status %d body %s", resp.status, resp.body)
	}
	var inv ordersports.InvoiceOrderResponse
	if err := json.Unmarshal(resp.body, &inv); err != nil {
		t.Fatalf("invoice decode: %v", err)
	}
	if inv.NumberDisplay != "INV/2026-27/00001" {
		t.Fatalf("invoice number %q want INV/2026-27/00001", inv.NumberDisplay)
	}

	// Second invoice attempt → 409 (one per order).
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/invoice", tok, nil)
	if resp.status != http.StatusConflict {
		t.Fatalf("re-invoice: status %d want 409 body %s", resp.status, resp.body)
	}

	// Invoice read mirrors the order totals.
	resp = f.authedJSON(t, http.MethodGet, orderPath+"/invoice", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get invoice: status %d body %s", resp.status, resp.body)
	}
	var invDto ordersports.InvoiceDto
	if err := json.Unmarshal(resp.body, &invDto); err != nil {
		t.Fatalf("get invoice decode: %v", err)
	}
	if invDto.GrandTotalPaise != 100800 || invDto.NumberDisplay != inv.NumberDisplay {
		t.Fatalf("invoice dto total=%d number=%s", invDto.GrandTotalPaise, invDto.NumberDisplay)
	}

	// Payments list shows the token receipt.
	resp = f.authedJSON(t, http.MethodGet, orderPath+"/payments", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list payments: status %d body %s", resp.status, resp.body)
	}
	var pays ordersports.ListPaymentsResponse
	if err := json.Unmarshal(resp.body, &pays); err != nil {
		t.Fatalf("list payments decode: %v", err)
	}
	if len(pays.Payments) != 1 || pays.Payments[0].Kind != "token" || pays.Payments[0].AmountPaise != 25000 {
		t.Fatalf("payments = %+v want one token 25000", pays.Payments)
	}

	mustGet("post-invoice")
	if ord.State != "invoiced" || ord.InvoiceID == "" {
		t.Fatalf("order state=%s invoiceID=%q want invoiced + linked", ord.State, ord.InvoiceID)
	}
}

// TestE2E_Orders_CancelAndGuards covers the unhappy paths the lifecycle test
// skips: cancel requires a reason (400), cancel persists reason + terminal
// state (204), out-of-order transitions map to 409, and the whole surface is
// 401 without a bearer token.
func TestE2E_Orders_CancelAndGuards(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "orderscancel")
	tok := owner.AccessToken

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
	orderPath := "/api/v1/orders/orders/" + approved.OrderID

	// Skipping states → 409 invalid transition (pack from quotation_approved).
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/pack", tok, ordersports.PackOrderRequest{
		CarrierName: "BlueDart", BoxCount: 1, WeightGrams: 100,
	})
	if resp.status != http.StatusConflict {
		t.Fatalf("premature pack: status %d want 409 body %s", resp.status, resp.body)
	}

	// Cancel without reason → 400.
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("cancel no reason: status %d want 400 body %s", resp.status, resp.body)
	}

	// Cancel with reason → 204 + terminal state persisted.
	resp = f.authedJSON(t, http.MethodPost, orderPath+"/cancel", tok, ordersports.CancelOrderRequest{Reason: "customer withdrew"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("cancel: status %d body %s", resp.status, resp.body)
	}
	resp = f.authedJSON(t, http.MethodGet, orderPath, tok, nil)
	var ord ordersports.OrderDto
	if err := json.Unmarshal(resp.body, &ord); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ord.State != "cancelled" || ord.CancellationReason != "customer withdrew" {
		t.Fatalf("order state=%s reason=%q want cancelled", ord.State, ord.CancellationReason)
	}

	// Unauthenticated → 401 on read and write.
	resp = f.authedJSON(t, http.MethodGet, orderPath, "", nil)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("anon get order: status %d want 401", resp.status)
	}
	resp = f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations", "", ordersports.CreateQuotationRequest{})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("anon create quotation: status %d want 401", resp.status)
	}

	// Unknown order → 404.
	resp = f.authedJSON(t, http.MethodGet, "/api/v1/orders/orders/"+uuid.NewString(), tok, nil)
	if resp.status != http.StatusNotFound {
		t.Fatalf("missing order: status %d want 404", resp.status)
	}
}
