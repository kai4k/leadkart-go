//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// End-to-end tests for the commerce-integrity fixes (branch
// fix/commerce-integrity): refusals that used to be silent corruption or a
// 500 must now be a stable 422/409 with nothing written.

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

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
		BatchNumber: "B-" + sku, ManufactureDate: time.Now().AddDate(0, -3, 0), ExpiryDate: time.Now().AddDate(2, 0, 0),
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
