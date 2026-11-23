//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

// TestE2E_Orders_PaymentForUnknownOrderIs404: a payment needs an order the
// caller can see. orders.payments has no foreign key to orders.orders, so an
// unknown id (or another company's order) used to be accepted with 201 and
// left a payment row that belongs to no order.
func TestE2E_Orders_PaymentForUnknownOrderIs404(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "payunknown").AccessToken
	otherTok := f.registerAndLogin(t, "payother").AccessToken
	body := ordersports.RecordPaymentRequest{Kind: "full", Method: "upi", AmountPaise: 100}

	unknown := "/api/v1/orders/orders/" + uuid.NewString()
	resp := f.authedJSON(t, http.MethodPost, unknown+"/payments", tok, body)
	if resp.status != http.StatusNotFound || errorCode(t, resp.body) != "order_not_found" {
		t.Fatalf("unknown order: status %d body %s; want 404 order_not_found", resp.status, resp.body)
	}
	if n := ordersPaymentCount(t, f, tok, unknown); n != 0 {
		t.Fatalf("payments written for an unknown order = %d, want 0", n)
	}

	// Another company's order is as unknown as a random id.
	foreign := ordersApprovedOrder(t, f, otherTok)
	resp = f.authedJSON(t, http.MethodPost, foreign+"/payments", tok, body)
	if resp.status != http.StatusNotFound || errorCode(t, resp.body) != "order_not_found" {
		t.Fatalf("another company's order: status %d body %s; want 404 order_not_found", resp.status, resp.body)
	}
	if n := ordersPaymentCount(t, f, otherTok, foreign); n != 0 {
		t.Fatalf("payments on the other company's order = %d, want 0", n)
	}
	if n := ordersPaymentCount(t, f, tok, foreign); n != 0 {
		t.Fatalf("payments the caller wrote against the other company's order = %d, want 0", n)
	}

	// A real order of the caller still records, as before.
	own := ordersApprovedOrder(t, f, tok)
	resp = f.authedJSON(t, http.MethodPost, own+"/payments", tok, body)
	if resp.status != http.StatusCreated {
		t.Fatalf("own order: status %d body %s; want 201", resp.status, resp.body)
	}
	if n := ordersPaymentCount(t, f, tok, own); n != 1 {
		t.Fatalf("payments on the caller's order = %d, want 1", n)
	}
}
