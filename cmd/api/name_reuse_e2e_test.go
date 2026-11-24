//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// ADR 0073 addendum (owner ruling 2026-11-25): a deleted (inactive) role or drug schedule class
// frees its name, but a product SKU never frees up and says so distinctly.

package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"

	csports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
)

func TestE2E_RoleName_FreesUpOnDelete_AndReactivateCollides(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "rolereuse").AccessToken
	base := "/api/v1/roles"

	old := f.customRole(t, tok, "Area Manager", 60)
	// An active collision keeps the existing code.
	if r, _ := f.createRoleVia(t, tok, ports.CreateRoleRequest{Name: "Area Manager", HierarchyLevel: 60}); r.status != http.StatusConflict || invObj(t, r.body)["error"] != "role_name_taken" {
		t.Fatalf("active duplicate: %d %s, want 409 role_name_taken", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, base+"/"+old+"/deactivate", tok, nil); r.status != http.StatusOK {
		t.Fatalf("deactivate: %d %s", r.status, r.body)
	}
	// Deleted: the name is free.
	fresh := f.customRole(t, tok, "Area Manager", 60)
	// The old one cannot come back under a taken name.
	r := f.authedJSON(t, http.MethodPost, base+"/"+old+"/reactivate", tok, nil)
	if r.status != http.StatusConflict || invObj(t, r.body)["error"] != "role_name_taken" {
		t.Fatalf("reactivate over a taken name: %d %s, want 409 role_name_taken", r.status, r.body)
	}
	// Free the name again: now it can.
	if r := f.authedJSON(t, http.MethodPost, base+"/"+fresh+"/deactivate", tok, nil); r.status != http.StatusOK {
		t.Fatalf("deactivate fresh: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, base+"/"+old+"/reactivate", tok, nil); r.status != http.StatusOK {
		t.Fatalf("reactivate once free: %d %s", r.status, r.body)
	}
}

func TestE2E_ScheduleClassName_FreesUpOnDelete_AndReactivateCollides(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "classreuse").AccessToken
	post := func(path string, body map[string]any) httpResp {
		return f.authedJSON(t, http.MethodPost, path, tok, body)
	}

	old := csDecode[csports.ScheduleClassesDto](t, post(csClasses, map[string]any{"label": "Controlled", "reason": "local rule"}), http.StatusCreated).Class
	if dup := post(csClasses, map[string]any{"label": "controlled", "reason": "again"}); dup.status != http.StatusUnprocessableEntity {
		t.Fatalf("active duplicate: %d %s, want the existing 422 duplicate", dup.status, dup.body)
	}
	csDecode[csports.ScheduleClassesDto](t, post(csClasses+"/"+old.ID+"/deactivate", map[string]any{"reason": "retired"}), http.StatusOK)
	fresh := csDecode[csports.ScheduleClassesDto](t, post(csClasses, map[string]any{"label": "Controlled", "reason": "new"}), http.StatusCreated).Class

	r := post(csClasses+"/"+old.ID+"/reactivate", map[string]any{"reason": "back"})
	if r.status != http.StatusConflict || invObj(t, r.body)["error"] != "schedule_class_name_taken" {
		t.Fatalf("reactivate over a taken name: %d %s, want 409 schedule_class_name_taken", r.status, r.body)
	}
	csDecode[csports.ScheduleClassesDto](t, post(csClasses+"/"+fresh.ID+"/deactivate", map[string]any{"reason": "retired"}), http.StatusOK)
	csDecode[csports.ScheduleClassesDto](t, post(csClasses+"/"+old.ID+"/reactivate", map[string]any{"reason": "back"}), http.StatusOK)
}

func TestE2E_ProductSKU_NeverFreesUp_AndSaysWhenAnInactiveProductHoldsIt(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "skureuse").AccessToken
	base := "/api/v1/inventory/products"
	rate := e2eRate(t, f, tok, 1800)
	create := func(sku string) httpResp {
		return f.authedJSON(t, http.MethodPost, base, tok, invProductBody(sku, map[string]any{"gst_rate_id": rate}))
	}

	id := invCreateProduct(t, f, tok, "SKU-X", nil)
	if r := create("SKU-X"); r.status != http.StatusConflict || invObj(t, r.body)["error"] != "already_exists" {
		t.Fatalf("active holder: %d %s, want 409 already_exists", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, base+"/"+id+"/deactivate", tok, nil); r.status != http.StatusOK {
		t.Fatalf("deactivate: %d %s", r.status, r.body)
	}
	if r := create("SKU-X"); r.status != http.StatusConflict || invObj(t, r.body)["error"] != "product_sku_taken_by_inactive" {
		t.Fatalf("inactive holder: %d %s, want 409 product_sku_taken_by_inactive", r.status, r.body)
	}
}
