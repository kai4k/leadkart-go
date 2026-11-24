//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// ADR 0073 (owner ruling 2026-11-24): products, roles and drug schedule classes go inactive and
// come back; none is deleted. Each resource: deactivate answers the updated record, the default
// list hides it, ?status=inactive shows it, the effect is enforced, reactivate undoes it.

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
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

// effectivePermissions is what the caller's token holds right now, from the capabilities read.
func (f e2eFixture) effectivePermissions(t *testing.T, token string) []string {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", token, nil)
	var c ports.CapabilitiesDto
	if r.status != http.StatusOK || json.Unmarshal(r.body, &c) != nil {
		t.Fatalf("capabilities: %d %s", r.status, r.body)
	}
	return c.EffectivePermissions
}

func TestE2E_Role_InactiveNotDeleted(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "roleinact")
	tok := owner.AccessToken
	keep := f.customRole(t, tok, "Keep Role", 60)
	off := f.customRole(t, tok, "Off Role", 60)
	member := f.createMember(t, owner, "assignee")
	base := "/api/v1/roles"

	if r := f.roleWrite(t, http.MethodDelete, base+"/"+off, tok, nil); r.status != http.StatusMethodNotAllowed && r.status != http.StatusNotFound {
		t.Fatalf("DELETE role: status %d, want the route gone (404/405)", r.status)
	}

	// Deactivate answers the updated role; repeating answers the same (a no-op).
	for range 2 {
		r := f.roleWrite(t, http.MethodPost, base+"/"+off+"/deactivate", tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("deactivate: status %d body %s", r.status, r.body)
		}
		invWant(t, invObj(t, r.body), "is_active", false)
	}

	// Hidden from the default list, shown by status=inactive, both by status=all; counts whatever the filter.
	ids, counts := lifecycleList(t, f, tok, base, "", "roles", "id")
	if containsID(ids, off) || !containsID(ids, keep) {
		t.Fatalf("default list: want the active role and not the inactive one")
	}
	if counts == nil || counts["inactive"] != float64(1) || counts["active"].(float64) < 2 {
		t.Fatalf("status_counts %v, want 1 inactive and the active roles", counts)
	}
	if ids, _ = lifecycleList(t, f, tok, base, "inactive", "roles", "id"); len(ids) != 1 || ids[0] != off {
		t.Fatalf("status=inactive %v, want only the inactive role", ids)
	}
	if ids, _ = lifecycleList(t, f, tok, base, "all", "roles", "id"); !containsID(ids, off) || !containsID(ids, keep) {
		t.Fatalf("status=all %v, want both", ids)
	}
	if r := f.authedJSON(t, http.MethodGet, base+"?status=deleted", tok, nil); r.status != http.StatusBadRequest {
		t.Fatalf("status=deleted: %d, want 400 invalid_status_filter", r.status)
	}

	// Effect: still readable, takes no new holder; reactivate makes it assignable again.
	if got := f.roleDto(t, tok, off); got.IsActive {
		t.Fatal("GET of an inactive role reads as active")
	}
	r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+member.membershipID+"/roles", tok, ports.AssignUserRoleRequest{RoleID: off})
	if r.status != http.StatusConflict || invObj(t, r.body)["error"] != "role_inactive" {
		t.Fatalf("assign an inactive role: %d %s, want 409 role_inactive", r.status, r.body)
	}
	for range 2 {
		r := f.roleWrite(t, http.MethodPost, base+"/"+off+"/reactivate", tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("reactivate: status %d body %s", r.status, r.body)
		}
		invWant(t, invObj(t, r.body), "is_active", true)
	}
	if ids, _ = lifecycleList(t, f, tok, base, "", "roles", "id"); !containsID(ids, off) {
		t.Fatal("default list after reactivate: the role is not back")
	}
	f.assignRole(t, tok, member.membershipID, off)

	// A system role cannot be made inactive; an unknown role is 404.
	system := f.roleIDByName(t, tok, "Company Owner")
	if r := f.roleWrite(t, http.MethodPost, base+"/"+system+"/deactivate", tok, nil); r.status != http.StatusConflict {
		t.Fatalf("deactivate a system role: %d %s, want 409", r.status, r.body)
	}
	for _, verb := range []string{"deactivate", "reactivate"} {
		if r := f.roleWrite(t, http.MethodPost, base+"/019f0000-0000-7000-8000-000000000000/"+verb, tok, nil); r.status != http.StatusNotFound {
			t.Fatalf("%s unknown role: %d, want 404", verb, r.status)
		}
	}
}
