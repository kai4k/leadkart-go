//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup;
// per-request HTTP uses t.Context().

// End-to-end HTTP contract test for the Dispatch surface: consignment-note
// create (manual route) → reads → carrier status lifecycle through the real
// ServeMux + authz gates.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
)

// TestE2E_Dispatch_ConsignmentLifecycle drives create → get-by-order →
// dispatch(docket) → in-transit → delivered, then proves terminal-state 409
// and idempotent re-create (200 already_existed).
func TestE2E_Dispatch_ConsignmentLifecycle(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "dispatchflow")
	tok := owner.AccessToken
	orderID := uuid.NewString()

	// Create the slot.
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
		OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create: status %d body %s", resp.status, resp.body)
	}
	var created dispatchports.CreateConsignmentNoteResponse
	if err := json.Unmarshal(resp.body, &created); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	if created.AlreadyExisted {
		t.Fatal("fresh create flagged already_existed")
	}

	// Idempotent re-create for the same order → 200 + already_existed.
	resp = f.authedJSON(t, http.MethodPost, "/api/v1/dispatch/consignment-notes", tok, dispatchports.CreateConsignmentNoteRequest{
		OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
	})
	if resp.status != http.StatusOK {
		t.Fatalf("re-create: status %d want 200 body %s", resp.status, resp.body)
	}

	// Get by order.
	resp = f.authedJSON(t, http.MethodGet, "/api/v1/dispatch/consignment-notes?order_id="+orderID, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get by order: status %d body %s", resp.status, resp.body)
	}
	var note dispatchports.ConsignmentNoteDto
	if err := json.Unmarshal(resp.body, &note); err != nil {
		t.Fatalf("get decode: %v", err)
	}
	if note.Status != "pending" || note.ID != created.ConsignmentNoteID {
		t.Fatalf("note status=%s id=%s want pending/%s", note.Status, note.ID, created.ConsignmentNoteID)
	}

	base := "/api/v1/dispatch/consignment-notes/" + note.ID

	// pending → dispatched (docket) → in_transit → delivered.
	resp = f.authedJSON(t, http.MethodPost, base+"/dispatch", tok, dispatchports.MarkDispatchedRequest{DocketNumber: "BD-12345"})
	if resp.status != http.StatusNoContent {
		t.Fatalf("dispatch: status %d body %s", resp.status, resp.body)
	}
	for _, step := range []string{"/in-transit", "/delivered"} {
		resp = f.authedJSON(t, http.MethodPost, base+step, tok, nil)
		if resp.status != http.StatusNoContent {
			t.Fatalf("%s: status %d body %s", step, resp.status, resp.body)
		}
	}

	resp = f.authedJSON(t, http.MethodGet, base, tok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get by id: status %d body %s", resp.status, resp.body)
	}
	if err := json.Unmarshal(resp.body, &note); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if note.Status != "delivered" || note.DocketNumber != "BD-12345" {
		t.Fatalf("note status=%s docket=%s want delivered/BD-12345", note.Status, note.DocketNumber)
	}

	// Terminal guard: failing a delivered note → 409.
	resp = f.authedJSON(t, http.MethodPost, base+"/failed", tok, dispatchports.MarkFailedRequest{Reason: "late"})
	if resp.status != http.StatusConflict {
		t.Fatalf("fail delivered: status %d want 409 body %s", resp.status, resp.body)
	}

	// Anonymous → 401.
	resp = f.authedJSON(t, http.MethodGet, base, "", nil)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("anon get: status %d want 401", resp.status)
	}
}
