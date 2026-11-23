//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	expiryPkg "github.com/leadkart/leadkart-go/internal/common/expiry"
)

// invAddBatch adds a batch expiring on expiry; extra keys are merged into the body.
func invAddBatch(t *testing.T, f e2eFixture, tok, productID, number string, expiry time.Time, extra map[string]any) httpResp {
	t.Helper()
	body := map[string]any{
		"batch_number":      number,
		"manufacture_date":  time.Now().UTC().AddDate(-2, 0, 0).Format(time.RFC3339),
		"expiry_month":      expiryPkg.PrintedMonth(expiry),
		"manufacturer_name": "Acme", "manufacturing_licence_number": "ML-1",
		"mrp_paise": 25000, "purchase_price_paise": 18000,
	}
	for k, v := range extra {
		body[k] = v
	}
	return f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products/"+productID+"/batches", tok, body)
}

// invBatchWithStock adds a batch and moves qty units in with the given movement type.
func invBatchWithStock(t *testing.T, f e2eFixture, tok, productID, number string, expiry time.Time, moveType string, qty int) string {
	t.Helper()
	resp := invAddBatch(t, f, tok, productID, number, expiry, nil)
	if resp.status != http.StatusCreated {
		t.Fatalf("add batch %s: status %d body %s", number, resp.status, resp.body)
	}
	id, _ := invObj(t, resp.body)["batch_id"].(string)
	resp = f.authedJSON(t, http.MethodPost, "/api/v1/inventory/batches/"+id+"/movements", tok,
		map[string]any{"type": moveType, "quantity": qty, "reason": "test stock"})
	if resp.status != http.StatusCreated {
		t.Fatalf("stock batch %s: status %d body %s", number, resp.status, resp.body)
	}
	return id
}

// invListProducts returns the product list for the query string, keyed by id.
func invListProducts(t *testing.T, f e2eFixture, tok, query string) map[string]map[string]any {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products?"+query, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list products ?%s: status %d body %s", query, resp.status, resp.body)
	}
	items, _ := invObj(t, resp.body)["items"].([]any)
	out := make(map[string]map[string]any, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		id, _ := m["id"].(string)
		out[id] = m
	}
	return out
}

// invWantIDs fails unless the listed products are exactly want.
func invWantIDs(t *testing.T, label string, got map[string]map[string]any, want ...string) {
	t.Helper()
	ids := make([]string, 0, len(got))
	for id := range got {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("%s: products %v, want %v", label, ids, want)
	}
}

// TestE2E_Inventory_ProductStockTotalsAndFilters: the product list and detail
// carry the stock totals and the earliest expiry, and the list filters on them.
func TestE2E_Inventory_ProductStockTotalsAndFilters(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "invstock").AccessToken
	now := time.Now().UTC()
	month := func(plus int) time.Time { return expiryPkg.MonthEnd(now.Year(), now.Month()+time.Month(plus)) }

	// A: 40 units printed two months ahead (still for sale: the default stop is the
	// start of the printed month), 20 fourteen months ahead, 5 in a printed month that
	// is over; reorder level 100.
	a := invCreateProduct(t, f, tok, "STK-A", map[string]any{"product_category": "Cardiac", "reorder_level": 100})
	soon := invBatchWithStock(t, f, tok, a, "A-SOON", month(2), "inbound", 40)
	invBatchWithStock(t, f, tok, a, "A-LATE", month(14), "inbound", 20)
	invBatchWithStock(t, f, tok, a, "A-GONE", month(-1), "adjustment", 5)
	// B: plenty of stock, no reorder level. C: no batches, reorder level 10.
	b := invCreateProduct(t, f, tok, "STK-B", nil)
	invBatchWithStock(t, f, tok, b, "B-1", month(14), "inbound", 500)
	c := invCreateProduct(t, f, tok, "STK-C", map[string]any{"reorder_level": 10})

	// Detail: the five buckets; expired units are on hand but not for sale; the earliest
	// expiry is the soonest SELLABLE batch holding stock.
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+soon, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get batch: status %d body %s", resp.status, resp.body)
	}
	soonBatch := invObj(t, resp.body)
	invWant(t, soonBatch, "quantity_on_hand", 40)
	invWant(t, soonBatch, "quantity_reserved", 0)

	pa := invGetProduct(t, f, tok, a)
	invWant(t, pa, "stock_on_hand", 65)
	invWant(t, pa, "stock_reserved", 0)
	invWant(t, pa, "stock_available", 60)
	invWant(t, pa, "stock_unsellable", 5)
	invWant(t, pa, "stock_quarantined", 0)
	invWant(t, pa, "earliest_expiry", soonBatch["expiry_date"])
	pc := invGetProduct(t, f, tok, c)
	invWant(t, pc, "stock_on_hand", 0)
	invWant(t, pc, "stock_available", 0)
	if v, present := pc["earliest_expiry"]; present {
		t.Fatalf("a product with no stock has earliest_expiry %v, want it absent", v)
	}

	// List: same numbers, one read.
	all := invListProducts(t, f, tok, "")
	invWantIDs(t, "no filter", all, a, b, c)
	invWant(t, all[a], "stock_on_hand", 65)
	invWant(t, all[a], "stock_unsellable", 5)
	invWant(t, all[a], "earliest_expiry", soonBatch["expiry_date"])
	invWant(t, all[b], "stock_available", 500)
	invWant(t, all[c], "reorder_level", 10)

	// Filters.
	invWantIDs(t, "low_stock", invListProducts(t, f, tok, "low_stock=true"), a, c)
	invWantIDs(t, "low_stock=false", invListProducts(t, f, tok, "low_stock=false"), a, b, c)
	invWantIDs(t, "expiring_within_days=70", invListProducts(t, f, tok, "expiring_within_days=70"), a)
	invWantIDs(t, "expiring_within_days=3650", invListProducts(t, f, tok, "expiring_within_days=3650"), a, b)
	invWantIDs(t, "expiring_within_days=0", invListProducts(t, f, tok, "expiring_within_days=0"))
	invWantIDs(t, "category", invListProducts(t, f, tok, "category=cardiac"), a)
	invWantIDs(t, "category + low_stock", invListProducts(t, f, tok, "category=General&low_stock=true"), c)
	invWantIDs(t, "search", invListProducts(t, f, tok, "search=stk-b"), b)

	// A filter the server cannot read is refused by name, never ignored.
	for _, bad := range []struct{ query, field, code string }{
		{"low_stock=maybe", "low_stock", "invalid_type"},
		{"expiring_within_days=soon", "expiring_within_days", "invalid_type"},
		{"expiring_within_days=-1", "expiring_within_days", "too_small"},
		{"expiring_within_days=36501", "expiring_within_days", "too_large"},
		{"category=" + strings.Repeat("c", 65), "category", "too_long"},
	} {
		resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products?"+bad.query, tok, nil)
		if resp.status != http.StatusBadRequest || errorCode(t, resp.body) != "invalid_query" {
			t.Fatalf("?%s: status %d body %s; want 400 invalid_query", bad.query, resp.status, resp.body)
		}
		if got := invFieldCodes(t, resp.body, bad.field); !slices.Equal(got, []string{bad.code}) {
			t.Fatalf("?%s: errors.%s = %v, want [%s]", bad.query, bad.field, got, bad.code)
		}
	}

	// Every batch read carries quantity_reserved.
	for _, path := range []string{"/batches?include_expired=true", "/fefo-batches"} {
		resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products/"+a+path, tok, nil)
		if resp.status != http.StatusOK {
			t.Fatalf("%s: status %d body %s", path, resp.status, resp.body)
		}
		items, _ := invObj(t, resp.body)["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("%s: no batches", path)
		}
		for _, it := range items {
			m, _ := it.(map[string]any)
			invWant(t, m, "quantity_reserved", 0)
		}
	}

	// Another company sees none of it.
	other := f.registerAndLogin(t, "invstockother").AccessToken
	invWantIDs(t, "other company", invListProducts(t, f, other, ""))
	if resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products/"+a, other, nil); resp.status != http.StatusNotFound {
		t.Fatalf("other company reads product: status %d, want 404", resp.status)
	}
}
