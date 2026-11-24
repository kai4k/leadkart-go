//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// ADR 0073 (owner ruling 2026-11-24): products, roles and drug schedule classes go inactive and
// come back; none is deleted. Each resource: deactivate answers the updated record, the default
// list hides it, ?status=inactive shows it, the effect is enforced, reactivate undoes it.

package main

import (
	"net/http"
	"testing"
	"time"
)

// lifecycleList lists path with ?status= (empty: no parameter) and returns the ids of the
// records under itemsKey plus the status_counts of the response (nil when absent).
func lifecycleList(t *testing.T, f e2eFixture, tok, path, status, itemsKey, idKey string) (ids []string, counts map[string]any) {
	t.Helper()
	url := path
	if status != "" {
		url += "?status=" + status
	}
	resp := f.authedJSON(t, http.MethodGet, url, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("GET %s: status %d body %s", url, resp.status, resp.body)
	}
	body := invObj(t, resp.body)
	items, _ := body[itemsKey].([]any)
	for _, it := range items {
		row, _ := it.(map[string]any)
		id, _ := row[idKey].(string)
		ids = append(ids, id)
	}
	counts, _ = body["status_counts"].(map[string]any)
	return ids, counts
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func TestE2E_Product_InactiveNotDeleted(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "prodinact").AccessToken
	keep := invCreateProduct(t, f, tok, "KEEP-1", nil)
	gone := invCreateProduct(t, f, tok, "OFF-1", nil)
	live := time.Now().UTC().AddDate(1, 0, 0)
	base := "/api/v1/inventory/products"

	// A batch with stock exists before the product goes inactive; it must survive.
	resp := invAddBatch(t, f, tok, gone, "BEFORE", live, map[string]any{"initial_quantity": 7})
	if resp.status != http.StatusCreated {
		t.Fatalf("seed batch: %d %s", resp.status, resp.body)
	}

	// The DELETE route is gone.
	if r := f.authedJSON(t, http.MethodDelete, base+"/"+gone, tok, nil); r.status != http.StatusMethodNotAllowed && r.status != http.StatusNotFound {
		t.Fatalf("DELETE product: status %d, want the route gone (404/405)", r.status)
	}

	// Deactivate answers the updated product, and repeating it answers the same.
	for range 2 {
		r := f.authedJSON(t, http.MethodPost, base+"/"+gone+"/deactivate", tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("deactivate: status %d body %s", r.status, r.body)
		}
		invWant(t, invObj(t, r.body), "is_active", false)
	}

	// Hidden from the default list, shown by status=inactive, both by status=all.
	ids, counts := lifecycleList(t, f, tok, base, "", "items", "id")
	if containsID(ids, gone) || !containsID(ids, keep) {
		t.Fatalf("default list %v: want the active product only", ids)
	}
	if counts == nil || counts["active"] != float64(1) || counts["inactive"] != float64(1) {
		t.Fatalf("status_counts %v, want 1 active and 1 inactive whatever the filter", counts)
	}
	if ids, _ = lifecycleList(t, f, tok, base, "inactive", "items", "id"); len(ids) != 1 || ids[0] != gone {
		t.Fatalf("status=inactive %v, want only the inactive product", ids)
	}
	if ids, _ = lifecycleList(t, f, tok, base, "all", "items", "id"); len(ids) != 2 {
		t.Fatalf("status=all %v, want both", ids)
	}
	if r := f.authedJSON(t, http.MethodGet, base+"?status=deleted", tok, nil); r.status != http.StatusBadRequest {
		t.Fatalf("status=deleted: %d, want 400 invalid_status_filter", r.status)
	}

	// Effect: still readable with its stock, and it takes no new batch.
	p := invGetProduct(t, f, tok, gone)
	invWant(t, p, "is_active", false)
	invWant(t, p, "stock_on_hand", 7)
	r := invAddBatch(t, f, tok, gone, "AFTER", live, nil)
	if r.status != http.StatusConflict || invObj(t, r.body)["error"] != "product_inactive" {
		t.Fatalf("add batch to an inactive product: %d %s, want 409 product_inactive", r.status, r.body)
	}
	if n := invBatchCount(t, f, tok, gone); n != 1 {
		t.Fatalf("batches after the refused add: %d, want the 1 that existed", n)
	}

	// Reactivate: back in the default list, takes a batch again; repeating is a no-op 200.
	for range 2 {
		r := f.authedJSON(t, http.MethodPost, base+"/"+gone+"/reactivate", tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("reactivate: status %d body %s", r.status, r.body)
		}
		invWant(t, invObj(t, r.body), "is_active", true)
	}
	if ids, _ = lifecycleList(t, f, tok, base, "", "items", "id"); !containsID(ids, gone) {
		t.Fatalf("default list after reactivate %v, want it back", ids)
	}
	if r := invAddBatch(t, f, tok, gone, "AFTER", live, nil); r.status != http.StatusCreated {
		t.Fatalf("add batch after reactivate: %d %s", r.status, r.body)
	}

	// Unknown product: 404 on both transitions.
	missing := "019f0000-0000-7000-8000-000000000000"
	for _, verb := range []string{"deactivate", "reactivate"} {
		if r := f.authedJSON(t, http.MethodPost, base+"/"+missing+"/"+verb, tok, nil); r.status != http.StatusNotFound {
			t.Fatalf("%s unknown product: %d, want 404", verb, r.status)
		}
	}
}
