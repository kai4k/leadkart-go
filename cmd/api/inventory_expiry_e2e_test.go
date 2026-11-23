//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	csports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
)

func invMove(t *testing.T, f e2eFixture, tok, batchID, kind string, qty int) httpResp {
	t.Helper()
	return f.authedJSON(t, http.MethodPost, "/api/v1/inventory/batches/"+batchID+"/movements", tok,
		map[string]any{"type": kind, "quantity": qty, "reason": "e2e " + kind})
}

// The expiry month, the stock buckets and the quarantine flow through the API:
// a batch stores the last day of its printed month, set-apart units are never
// available, a write-off is a state change that keeps the batch and its history.
func TestE2E_Inventory_ExpiryMonthBucketsQuarantineAndWriteOff(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "invexpiry").AccessToken
	p := invCreateProduct(t, f, tok, "EXP-1", nil)
	now := time.Now().UTC()
	month := func(plus int) time.Time { return expiry.MonthEnd(now.Year(), now.Month()+time.Month(plus)) }

	// The label month is the contract: a malformed or missing one is a field error.
	for _, c := range []struct {
		month any
		code  string
	}{{"2027-13", "invalid_format"}, {"03/2027", "invalid_format"}, {"2027-03-31", "invalid_format"}, {"", "required"}} {
		r := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products/"+p+"/batches", tok, map[string]any{
			"batch_number": "BAD", "manufacture_date": now.AddDate(-1, 0, 0).Format(time.RFC3339), "expiry_month": c.month,
			"manufacturer_name": "Acme", "manufacturing_licence_number": "ML-1", "mrp_paise": 100, "purchase_price_paise": 50,
		})
		if r.status != http.StatusUnprocessableEntity || !slices.Equal(invFieldCodes(t, r.body, "expiry_month"), []string{c.code}) {
			t.Fatalf("expiry_month %v: %d %s, want 422 expiry_month [%s]", c.month, r.status, r.body, c.code)
		}
	}
	// A batch cannot have been made tomorrow.
	r := invAddBatch(t, f, tok, p, "FUTURE", month(12), map[string]any{"manufacture_date": now.AddDate(0, 0, 3).Format(time.RFC3339)})
	if r.status != http.StatusUnprocessableEntity || !slices.Equal(invFieldCodes(t, r.body, "manufacture_date"), []string{"in_future"}) {
		t.Fatalf("future manufacture date: %d %s", r.status, r.body)
	}

	// A sellable batch with opening stock: the month and its last day, no warning.
	add := invAddBatch(t, f, tok, p, "SELL", month(6), map[string]any{"initial_quantity": 10})
	if add.status != http.StatusCreated {
		t.Fatalf("add: %d %s", add.status, add.body)
	}
	body := invObj(t, add.body)
	if w, _ := body["warnings"].([]any); len(w) != 0 {
		t.Fatalf("a sellable batch warned: %v", w)
	}
	id, _ := body["batch_id"].(string)
	b := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+id, tok, nil).body)
	invWant(t, b, "expiry_month", expiry.PrintedMonth(month(6)))
	invWant(t, b, "expiry_date", month(6).Format("2006-01-02")+"T00:00:00Z")
	invWant(t, b, "sellable", true)
	invWant(t, b, "past_printed_expiry", false)
	invWant(t, b, "quantity_available", 10)

	// Quarantine 4: set apart, never available, still on hand.
	if q := invMove(t, f, tok, id, "quarantine", 4); q.status != http.StatusCreated {
		t.Fatalf("quarantine: %d %s", q.status, q.body)
	} else {
		invWant(t, invObj(t, q.body), "quantity_quarantined_after", 4)
	}
	pr := invGetProduct(t, f, tok, p)
	invWant(t, pr, "stock_on_hand", 10)
	invWant(t, pr, "stock_available", 6)
	invWant(t, pr, "stock_quarantined", 4)
	if r := invMove(t, f, tok, id, "outbound", 7); r.status != http.StatusConflict || errorCode(t, r.body) != "insufficient_stock" {
		t.Fatalf("shipping quarantined units: %d %s", r.status, r.body)
	}
	if r := invMove(t, f, tok, id, "quarantine", 7); r.status != http.StatusConflict {
		t.Fatalf("quarantining more than the free units: %d %s", r.status, r.body)
	}

	// Write-off: a state change; the batch and its history stay.
	if r := invMove(t, f, tok, id, "write_off", 5); r.status != http.StatusConflict || errorCode(t, r.body) != "insufficient_quarantined" {
		t.Fatalf("writing off more than is set apart: %d %s", r.status, r.body)
	}
	if r := invMove(t, f, tok, id, "write_off", 4); r.status != http.StatusCreated {
		t.Fatalf("write-off: %d %s", r.status, r.body)
	}
	b = invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+id, tok, nil).body)
	invWant(t, b, "quantity_on_hand", 6)
	invWant(t, b, "quantity_quarantined", 0)
	moves := invObj(t, f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+id+"/movements", tok, nil).body)["items"].([]any)
	var kinds []string
	for _, m := range moves {
		kinds = append(kinds, m.(map[string]any)["type"].(string))
	}
	for _, want := range []string{"inbound", "quarantine", "write_off"} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("ledger types %v: missing %s (history is kept)", kinds, want)
		}
	}

	// Stock received into a batch past the company cut-off (this printed month, default
	// stop: start of the printed month) is accepted with a warning and counts as unsellable.
	late := invAddBatch(t, f, tok, p, "LATE", month(0), map[string]any{"initial_quantity": 3})
	if late.status != http.StatusCreated {
		t.Fatalf("past the cut-off: %d %s", late.status, late.body)
	}
	if w, _ := invObj(t, late.body)["warnings"].([]any); len(w) != 1 || w[0] != "past_sellable_cutoff" {
		t.Fatalf("warnings = %v, want [past_sellable_cutoff]", w)
	}
	pr = invGetProduct(t, f, tok, p)
	invWant(t, pr, "stock_unsellable", 3)
	invWant(t, pr, "stock_available", 6)
}

// e2eRate adds a rate to the company GST list (the company confirms it by name) and
// returns its id: a product takes its rate ONLY from that list.
func e2eRate(t *testing.T, f e2eFixture, tok string, bps int) string {
	t.Helper()
	r := f.authedJSON(t, http.MethodPost, csRates, tok, map[string]any{
		"rate_bps": bps, "label": "GST " + time.Now().Format("150405.000000") + "-" + strconv.Itoa(bps), "confirmed_by": "A. Mehta (CA)",
		"confirmed_on": "2026-10-30", "reason": "e2e fixture",
	})
	if r.status != http.StatusCreated {
		t.Fatalf("add gst rate %d: %d %s", bps, r.status, r.body)
	}
	id, _ := invObj(t, r.body)["id"].(string)
	syncInventoryReplica(t, f, tok)
	return id
}

// syncInventoryReplica stores the company current policy version in the Inventory
// replica, as the policy-changed subscriber does in production.
func syncInventoryReplica(t *testing.T, f e2eFixture, tok string) {
	t.Helper()
	// In production the event stream (forwarder, router) carries the new policy version
	// to the Inventory replica, which the product handlers read; the e2e fixtures run
	// without that stream (it is proven in company_settings_replicas_e2e_test), so the
	// helper stores the current version in the replica as the subscriber would.
	cur := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	replica := inventoryadapters.NewPolicyReplicaRepository(f.Pool, pg.NewTransactor(f.Pool))
	if _, err := replica.StoreIfNewer(t.Context(), tenant.ID(cur.TenantID), commercepolicy.Versioned{Version: cur.Version, Policy: cur.Policy}); err != nil {
		t.Fatalf("store the policy in the inventory replica: %v", err)
	}
}
