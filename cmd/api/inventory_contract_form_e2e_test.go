//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// End-to-end tests for the inventory contract the rebuilt web screens need:
// what the product form sends and reads back, and the server-supplied category
// list. Bodies are plain maps, so a test states the wire names it expects.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// invObj decodes a JSON object body.
func invObj(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return m
}

// invFieldCodes returns the rule codes of one field in an error body.
func invFieldCodes(t *testing.T, body []byte, field string) []string {
	t.Helper()
	var e struct {
		Errors map[string][]string `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Errors[field]
}

// invProductBody is a valid create body with the given SKU; extra keys are merged in.
func invProductBody(sku string, extra map[string]any) map[string]any {
	b := map[string]any{
		"sku": sku, "name": "Product " + sku, "dosage_form": "tablet", "pack_size": "10x10", "hsn_code": "30049099",
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// invCreateProduct creates a product and returns its id.
func invCreateProduct(t *testing.T, f e2eFixture, tok, sku string, extra map[string]any) string {
	t.Helper()
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody(sku, extra))
	if resp.status != http.StatusCreated {
		t.Fatalf("create product %s: status %d body %s", sku, resp.status, resp.body)
	}
	id, _ := invObj(t, resp.body)["product_id"].(string)
	if id == "" {
		t.Fatalf("create product %s: no product_id in %s", sku, resp.body)
	}
	return id
}

// invGetProduct reads one product.
func invGetProduct(t *testing.T, f e2eFixture, tok, id string) map[string]any {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products/"+id, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get product: status %d body %s", resp.status, resp.body)
	}
	return invObj(t, resp.body)
}

// invWant fails unless got[key] equals want (numbers compare as float64).
func invWant(t *testing.T, got map[string]any, key string, want any) {
	t.Helper()
	if n, ok := want.(int); ok {
		want = float64(n)
	}
	if got[key] != want {
		t.Fatalf("%s = %v (%T), want %v; whole body: %v", key, got[key], got[key], want, got)
	}
}

// TestE2E_Inventory_ProductFormFields: category, reorder level and the
// expiry-alert threshold can be sent on create and update and are read back.
func TestE2E_Inventory_ProductFormFields(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "invform").AccessToken

	// Sent on create, read back; the category's default GST rate applies.
	id := invCreateProduct(t, f, tok, "FORM-1", map[string]any{
		"product_category": "Nutraceuticals", "reorder_level": 50, "expiry_alert_threshold_days": 30,
	})
	p := invGetProduct(t, f, tok, id)
	invWant(t, p, "product_category", "Nutraceuticals")
	invWant(t, p, "reorder_level", 50)
	invWant(t, p, "expiry_alert_threshold_days", 30)
	invWant(t, p, "gst_rate_bps", 1800)

	// Left out: the documented defaults.
	p = invGetProduct(t, f, tok, invCreateProduct(t, f, tok, "FORM-2", nil))
	invWant(t, p, "product_category", "General")
	invWant(t, p, "reorder_level", 0)
	invWant(t, p, "expiry_alert_threshold_days", 90)

	// An explicit 0 switches the expiry alert off; it is not replaced by 90.
	p = invGetProduct(t, f, tok, invCreateProduct(t, f, tok, "FORM-3", map[string]any{"expiry_alert_threshold_days": 0}))
	invWant(t, p, "expiry_alert_threshold_days", 0)

	// Update.
	path := "/api/v1/inventory/products/" + id
	resp := f.authedJSON(t, http.MethodPatch, path, tok, map[string]any{
		"product_category": "Diabetic", "reorder_level": 7, "expiry_alert_threshold_days": 45,
	})
	if resp.status != http.StatusNoContent {
		t.Fatalf("patch: status %d body %s", resp.status, resp.body)
	}
	p = invGetProduct(t, f, tok, id)
	invWant(t, p, "product_category", "Diabetic")
	invWant(t, p, "reorder_level", 7)
	invWant(t, p, "expiry_alert_threshold_days", 45)
	invWant(t, p, "gst_rate_bps", 1800) // a category change never rewrites the rate

	// Refusals name the field.
	long := strings.Repeat("c", 65)
	bad := []struct {
		name  string
		extra map[string]any
		field string
		code  string
	}{
		{"negative reorder level", map[string]any{"reorder_level": -1}, "reorder_level", "too_small"},
		{"reorder level over the column", map[string]any{"reorder_level": 2147483648}, "reorder_level", "too_large"},
		{"negative threshold", map[string]any{"expiry_alert_threshold_days": -1}, "expiry_alert_threshold_days", "too_small"},
		{"threshold over the limit", map[string]any{"expiry_alert_threshold_days": 36501}, "expiry_alert_threshold_days", "too_large"},
		{"category over 64 characters", map[string]any{"product_category": long}, "product_category", "too_long"},
		{"category with NUL", map[string]any{"product_category": "a\x00b"}, "product_category", "invalid_characters"},
	}
	for i, c := range bad {
		sku := "BAD-" + string(rune('A'+i))
		for _, call := range []struct {
			method, path string
			body         map[string]any
		}{
			{http.MethodPost, "/api/v1/inventory/products", invProductBody(sku, c.extra)},
			{http.MethodPatch, path, c.extra},
		} {
			resp := f.authedJSON(t, call.method, call.path, tok, call.body)
			if resp.status != http.StatusUnprocessableEntity || errorCode(t, resp.body) != "validation_failed" {
				t.Fatalf("%s (%s): status %d body %s; want 422 validation_failed", c.name, call.method, resp.status, resp.body)
			}
			if got := invFieldCodes(t, resp.body, c.field); !slices.Equal(got, []string{c.code}) {
				t.Fatalf("%s (%s): errors.%s = %v, want [%s]; body %s", c.name, call.method, c.field, got, c.code, resp.body)
			}
		}
	}
	// A refused update changed nothing.
	p = invGetProduct(t, f, tok, id)
	invWant(t, p, "reorder_level", 7)
	invWant(t, p, "product_category", "Diabetic")
}

// TestE2E_Inventory_ProductCategories: the form reads the categories and their
// default GST rates from the server (the seeded reference table, unchanged).
func TestE2E_Inventory_ProductCategories(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "invcat").AccessToken

	resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/product-categories", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("status %d body %s; want 200", resp.status, resp.body)
	}
	var out struct {
		Items []struct {
			Category          string `json:"category"`
			DefaultGSTRateBps *int   `json:"default_gst_rate_bps"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]int{
		"General": 1200, "Gynaecology": 1200, "Paediatrics": 1200, "Diabetic": 1200, "Cardiac": 1200,
		"Ortho": 1200, "Nephrology": 1200, "Ayurvedic": 1200, "Nutraceuticals": 1800,
	}
	if len(out.Items) != len(want) {
		t.Fatalf("categories = %d, want %d: %s", len(out.Items), len(want), resp.body)
	}
	names := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		names = append(names, it.Category)
		rate, ok := want[it.Category]
		if !ok || it.DefaultGSTRateBps == nil || *it.DefaultGSTRateBps != rate {
			t.Fatalf("category %q rate %v; want %d (seed unchanged): %s", it.Category, it.DefaultGSTRateBps, rate, resp.body)
		}
	}
	if !slices.IsSorted(names) {
		t.Fatalf("categories not in name order: %v", names)
	}

	// Without a token the list is not served.
	if resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/product-categories", "", nil); resp.status != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", resp.status)
	}
}
