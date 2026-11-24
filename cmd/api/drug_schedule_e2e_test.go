//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"net/http"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	csports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

const csClasses = "/api/v1/company/drug-schedule-classes"

func csClassList(t *testing.T, f e2eFixture, tok string) csports.ScheduleClassesDto {
	t.Helper()
	return csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodGet, csClasses, tok, nil), http.StatusOK)
}

func labelsOf(l csports.ScheduleClassesDto) []string {
	out := make([]string, 0, len(l.Items))
	for _, c := range l.Items {
		out = append(out, c.Label)
	}
	return out
}

// The drug schedule is the company's own data: the list starts from the market
// defaults, the company adds, renames and deactivates classes (never deletes), an inactive
// class cannot be newly set on a product but one that carries it keeps it, and the mode
// cannot go off while a product carries one.
func TestE2E_DrugSchedule_TheCompanyOwnsTheClassListAndTheModeRules(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "drugsched")
	tok := reg.AccessToken
	rate := e2eRate(t, f, tok, 1200)
	defaults := commercepolicy.Default().DrugSchedule.Classes

	list := csClassList(t, f, tok)
	if len(list.Items) != len(defaults) || list.Mode != commercepolicy.DrugScheduleOptional {
		t.Fatalf("starting list %v mode %q", labelsOf(list), list.Mode)
	}

	// Add, rename; a repeated label (any case) is refused with a stable code.
	added := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodPost, csClasses, tok, map[string]any{"label": "Controlled", "reason": "local rule"}), http.StatusCreated)
	if added.Class == nil || added.Class.Label != "Controlled" {
		t.Fatalf("added %+v", added)
	}
	dup := f.authedJSON(t, http.MethodPost, csClasses, tok, map[string]any{"label": "controlled", "reason": "again"})
	if dup.status != http.StatusUnprocessableEntity || !slices.Equal(csFieldErrors(t, dup)["label"], []string{"duplicate"}) {
		t.Fatalf("a repeated label: %d %s", dup.status, dup.body)
	}
	renamed := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodPatch, csClasses+"/"+added.Class.ID, tok, map[string]any{"label": "Restricted", "reason": "clearer"}), http.StatusOK)
	if renamed.Class.Label != "Restricted" || renamed.Class.ID != added.Class.ID {
		t.Fatalf("rename keeps the id: %+v", renamed.Class)
	}
	if r := f.authedJSON(t, http.MethodPost, csClasses, tok, map[string]any{"label": "No reason"}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("a change needs a reason: %d", r.status)
	}

	// A product carries the class (the Inventory replica follows the policy).
	syncInventoryReplica(t, f, tok)
	p := invCreateProduct(t, f, tok, "SCH-1", map[string]any{"gst_rate_id": rate, "drug_schedule": added.Class.ID})
	pr := invGetProduct(t, f, tok, p)
	invWant(t, pr, "drug_schedule", added.Class.ID)
	invWant(t, pr, "drug_schedule_label", "Restricted")

	// ADR 0073: deactivating a class in use is allowed. It is idempotent (the policy version only
	// moves once), the product keeps it and its label, and the default list hides it.
	deact := func(id string) csports.ScheduleClassesDto {
		return csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodPost, csClasses+"/"+id+"/deactivate", tok, map[string]any{"reason": "retired"}), http.StatusOK)
	}
	off1 := deact(added.Class.ID)
	if off1.Class == nil || off1.Class.Active {
		t.Fatalf("deactivate answers the class, now inactive: %+v", off1.Class)
	}
	if off2 := deact(added.Class.ID); off2.PolicyVersion != off1.PolicyVersion {
		t.Fatalf("a repeated deactivate moved the policy version %d -> %d", off1.PolicyVersion, off2.PolicyVersion)
	}
	if r := f.authedJSON(t, http.MethodPost, csClasses+"/"+defaults[0].ID+"/deactivate", tok, map[string]any{}); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("a deactivate needs a reason: %d", r.status)
	}
	if got := csClassList(t, f, tok); len(got.Items) != len(defaults) || got.StatusCounts == nil || got.StatusCounts.Inactive != 1 || got.StatusCounts.Active != len(defaults) {
		t.Fatalf("default list %v counts %+v: want the active classes only and 1 inactive in the counts", labelsOf(got), got.StatusCounts)
	}
	inactive := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodGet, csClasses+"?status=inactive", tok, nil), http.StatusOK)
	if len(inactive.Items) != 1 || inactive.Items[0].ID != added.Class.ID || inactive.Items[0].Active {
		t.Fatalf("status=inactive: %+v", inactive.Items)
	}
	if all := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodGet, csClasses+"?status=all", tok, nil), http.StatusOK); len(all.Items) != len(defaults)+1 {
		t.Fatalf("status=all: %v", labelsOf(all))
	}
	if r := f.authedJSON(t, http.MethodGet, csClasses+"?status=deleted", tok, nil); r.status != http.StatusBadRequest {
		t.Fatalf("status=deleted: %d, want 400", r.status)
	}
	if r := f.authedJSON(t, http.MethodDelete, csClasses+"/"+added.Class.ID, tok, map[string]any{"reason": "x"}); r.status != http.StatusNotFound && r.status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE a class: %d, want the route gone", r.status)
	}

	// Effect: the product that carries the class keeps it and its label; nobody else can take it.
	syncInventoryReplica(t, f, tok)
	pr = invGetProduct(t, f, tok, p)
	invWant(t, pr, "drug_schedule", added.Class.ID)
	invWant(t, pr, "drug_schedule_label", "Restricted")
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/inventory/products/"+p, tok, map[string]any{"drug_schedule": added.Class.ID}); r.status != http.StatusNoContent {
		t.Fatalf("a product keeps its inactive class: %d %s", r.status, r.body)
	}
	r := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody("SCH-IN", map[string]any{"gst_rate_id": rate, "drug_schedule": added.Class.ID}))
	if r.status != http.StatusUnprocessableEntity || !slices.Equal(invFieldCodes(t, r.body, "drug_schedule"), []string{"inactive"}) {
		t.Fatalf("a new product takes an inactive class: %d %s, want 422 drug_schedule [inactive]", r.status, r.body)
	}

	// Reactivate: selectable again, and back in the default list.
	react := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodPost, csClasses+"/"+added.Class.ID+"/reactivate", tok, map[string]any{"reason": "back in use"}), http.StatusOK)
	if react.Class == nil || !react.Class.Active {
		t.Fatalf("reactivate: %+v", react.Class)
	}
	syncInventoryReplica(t, f, tok)
	back := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody("SCH-BACK", map[string]any{"gst_rate_id": rate, "drug_schedule": added.Class.ID}))
	if back.status != http.StatusCreated {
		t.Fatalf("a reactivated class is selectable: %d %s", back.status, back.body)
	}
	backID, _ := invObj(t, back.body)["product_id"].(string)

	// The schedule mode cannot go off while a product carries a class; the SAVE is refused.
	cur := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	off := f.authedJSON(t, http.MethodPatch, csSettings, tok, csPatch(cur.Version, "retiring the field", map[string]map[string]any{"drug_schedule": {"mode": "off"}}))
	if off.status != http.StatusUnprocessableEntity || !slices.Equal(csFieldErrors(t, off)["drug_schedule.mode"], []string{"schedule_in_use"}) {
		t.Fatalf("mode off while a product carries a class: %d %s", off.status, off.body)
	}

	// Clear the product class (and the other product that took the reactivated class); the mode can then go off.
	for _, id := range []string{p, backID} {
		if r := f.authedJSON(t, http.MethodPatch, "/api/v1/inventory/products/"+id, tok, map[string]any{"drug_schedule": ""}); r.status != http.StatusNoContent {
			t.Fatalf("clearing the class: %d %s", r.status, r.body)
		}
	}
	deact(added.Class.ID)
	if got := csClassList(t, f, tok); len(got.Items) != len(defaults) {
		t.Fatalf("after the deactivation: %v", labelsOf(got))
	}
	cur = csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok, csPatch(cur.Version, "retiring the field", map[string]map[string]any{"drug_schedule": {"mode": "off"}})), http.StatusOK)

	// Inventory refuses a class under mode off as well: its replica lags the company
	// settings, so the two refusals are paired.
	syncInventoryReplica(t, f, tok)
	r = f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody("SCH-2", map[string]any{"gst_rate_id": rate, "drug_schedule": defaults[0].ID}))
	if r.status != http.StatusUnprocessableEntity || !slices.Equal(invFieldCodes(t, r.body, "drug_schedule"), []string{"schedule_off"}) {
		t.Fatalf("a class under mode off: %d %s", r.status, r.body)
	}

	// Required: the company chose it, so a product needs a class; nothing else is blocked.
	cur = csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok, csPatch(cur.Version, "now mandatory", map[string]map[string]any{"drug_schedule": {"mode": "required"}})), http.StatusOK)
	syncInventoryReplica(t, f, tok)
	r = f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody("SCH-3", map[string]any{"gst_rate_id": rate}))
	if r.status != http.StatusUnprocessableEntity || !slices.Equal(invFieldCodes(t, r.body, "drug_schedule"), []string{"required"}) {
		t.Fatalf("required without a class: %d %s", r.status, r.body)
	}
	// Every default class is just data: any of them is accepted, none is special-cased.
	for i, c := range csClassList(t, f, tok).Items {
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody("SCH-C"+string(rune('A'+i)), map[string]any{"gst_rate_id": rate, "drug_schedule": c.ID})); r.status != http.StatusCreated {
			t.Fatalf("class %q: %d %s", c.Label, r.status, r.body)
		}
	}
}

// The company may deactivate every default class; the product form then has none to pick, and
// every class is still there under status=all (nothing is deleted).
func TestE2E_DrugSchedule_EveryClassCanBeDeactivatedAndNoneIsDeleted(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "drugsched2").AccessToken
	all := csClassList(t, f, tok).Items
	for _, c := range all {
		csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodPost, csClasses+"/"+c.ID+"/deactivate", tok, map[string]any{"reason": "this company does not use them"}), http.StatusOK)
	}
	got := csClassList(t, f, tok)
	if len(got.Items) != 0 || got.StatusCounts == nil || got.StatusCounts.Inactive != len(all) || got.StatusCounts.Active != 0 {
		t.Fatalf("list after deactivating every class: %v counts %+v", labelsOf(got), got.StatusCounts)
	}
	every := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodGet, csClasses+"?status=all", tok, nil), http.StatusOK)
	if len(every.Items) != len(all) {
		t.Fatalf("status=all %v, want every class kept", labelsOf(every))
	}
	for _, verb := range []string{"deactivate", "reactivate"} {
		if r := f.authedJSON(t, http.MethodPost, csClasses+"/00000000-0000-4000-8000-0000000000aa/"+verb, tok, map[string]any{"reason": "x"}); r.status != http.StatusNotFound {
			t.Fatalf("%s an unknown class: %d", verb, r.status)
		}
	}
}

// Review G2 mutant: the class list is company data guarded by tax.manage on every write.
// A member without it is refused on create, rename, deactivate and reactivate (403), and the list is untouched.
func TestE2E_DrugSchedule_ClassWritesNeedTheTaxManagePermission(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "classgate")
	// An admin-tier role holding company_settings.manage but NOT tax.manage: the nearest
	// grant, which must not open the class list.
	cp := permission.IdentityPermissions.CompanySettings
	plain := csTokenAt(t, f, owner, 10, cp.Read, cp.Manage)
	before := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodGet, csClasses, owner.AccessToken, nil), http.StatusOK)
	if len(before.Items) == 0 {
		t.Fatal("the company starts with a default class list")
	}

	if r := f.authedJSON(t, http.MethodPost, csClasses, plain, map[string]any{"label": "Sneaky", "reason": "no right"}); r.status != http.StatusForbidden {
		t.Fatalf("create without tax.manage: %d %s, want 403", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPatch, csClasses+"/"+before.Items[0].ID, plain, map[string]any{"label": "Renamed", "reason": "no right"}); r.status != http.StatusForbidden {
		t.Fatalf("rename without tax.manage: %d %s, want 403", r.status, r.body)
	}
	for _, verb := range []string{"deactivate", "reactivate"} {
		if r := f.authedJSON(t, http.MethodPost, csClasses+"/"+before.Items[0].ID+"/"+verb, plain, map[string]any{"reason": "no right"}); r.status != http.StatusForbidden {
			t.Fatalf("%s without tax.manage: %d %s, want 403", verb, r.status, r.body)
		}
	}
	after := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodGet, csClasses, owner.AccessToken, nil), http.StatusOK)
	if len(after.Items) != len(before.Items) {
		t.Fatalf("a refused write changed the list: %d -> %d", len(before.Items), len(after.Items))
	}
}
