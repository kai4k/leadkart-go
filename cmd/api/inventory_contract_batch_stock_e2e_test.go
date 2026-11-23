//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"net/http"
	"slices"
	"testing"
	"time"
)

// invBatchCount counts a product's batches, expired ones included.
func invBatchCount(t *testing.T, f e2eFixture, tok, productID string) int {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products/"+productID+"/batches?include_expired=true", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list batches: status %d body %s", resp.status, resp.body)
	}
	items, _ := invObj(t, resp.body)["items"].([]any)
	return len(items)
}

// invBatchState returns a batch's on-hand quantity and its ledger rows.
func invBatchState(t *testing.T, f e2eFixture, tok, batchID string) (onHand float64, movements []map[string]any) {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get batch: status %d body %s", resp.status, resp.body)
	}
	onHand, _ = invObj(t, resp.body)["quantity_on_hand"].(float64)
	resp = f.authedJSON(t, http.MethodGet, "/api/v1/inventory/batches/"+batchID+"/movements", tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list movements: status %d body %s", resp.status, resp.body)
	}
	items, _ := invObj(t, resp.body)["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		movements = append(movements, m)
	}
	return onHand, movements
}

// TestE2E_Inventory_AddBatchWithInitialStock is audit finding C6: a batch and
// its first stock are written together or not at all.
func TestE2E_Inventory_AddBatchWithInitialStock(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "invinit").AccessToken
	productID := invCreateProduct(t, f, tok, "INIT-1", nil)
	live := time.Now().UTC().AddDate(1, 0, 0)

	// With stock: one batch, one inbound ledger row, in one call.
	resp := invAddBatch(t, f, tok, productID, "WITH-STOCK", live, map[string]any{"initial_quantity": 25})
	if resp.status != http.StatusCreated {
		t.Fatalf("with stock: status %d body %s", resp.status, resp.body)
	}
	batchID, _ := invObj(t, resp.body)["batch_id"].(string)
	onHand, moves := invBatchState(t, f, tok, batchID)
	if onHand != 25 || len(moves) != 1 {
		t.Fatalf("with stock: on hand %v, %d ledger rows; want 25 and 1", onHand, len(moves))
	}
	invWant(t, moves[0], "type", "inbound")
	invWant(t, moves[0], "quantity", 25)
	invWant(t, moves[0], "quantity_on_hand_after", 25)
	invWant(t, invGetProduct(t, f, tok, productID), "stock_on_hand", 25)

	// Without the field, and with 0: unchanged behaviour, an empty batch.
	for number, extra := range map[string]map[string]any{"NO-FIELD": nil, "ZERO": {"initial_quantity": 0}} {
		resp := invAddBatch(t, f, tok, productID, number, live, extra)
		if resp.status != http.StatusCreated {
			t.Fatalf("%s: status %d body %s", number, resp.status, resp.body)
		}
		id, _ := invObj(t, resp.body)["batch_id"].(string)
		if onHand, moves := invBatchState(t, f, tok, id); onHand != 0 || len(moves) != 0 {
			t.Fatalf("%s: on hand %v, %d ledger rows; want 0 and 0", number, onHand, len(moves))
		}
	}
	before := invBatchCount(t, f, tok, productID)

	// A negative quantity is refused by field; no batch is left behind.
	resp = invAddBatch(t, f, tok, productID, "NEGATIVE", live, map[string]any{"initial_quantity": -5})
	if resp.status != http.StatusUnprocessableEntity || errorCode(t, resp.body) != "validation_failed" {
		t.Fatalf("negative: status %d body %s; want 422 validation_failed", resp.status, resp.body)
	}
	if got := invFieldCodes(t, resp.body, "initial_quantity"); !slices.Equal(got, []string{"too_small"}) {
		t.Fatalf("negative: errors.initial_quantity = %v, want [too_small]", got)
	}

	// Stock cannot be received into an expired batch (the existing inbound rule):
	// the whole request is refused, so the batch does not half-exist.
	resp = invAddBatch(t, f, tok, productID, "EXPIRED", time.Now().UTC().AddDate(0, 0, -1), map[string]any{"initial_quantity": 5})
	if resp.status != http.StatusConflict || errorCode(t, resp.body) != "batch_expired" {
		t.Fatalf("expired with stock: status %d body %s; want 409 batch_expired", resp.status, resp.body)
	}
	if after := invBatchCount(t, f, tok, productID); after != before {
		t.Fatalf("batches after two refused requests = %d, want %d (nothing written)", after, before)
	}
	// The refused numbers are still free.
	if resp := invAddBatch(t, f, tok, productID, "EXPIRED", live, map[string]any{"initial_quantity": 5}); resp.status != http.StatusCreated {
		t.Fatalf("reusing a refused batch number: status %d body %s; want 201", resp.status, resp.body)
	}
}
