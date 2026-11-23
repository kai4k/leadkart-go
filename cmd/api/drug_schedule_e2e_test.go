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
// defaults, the company adds, renames and removes classes, a class a product
// carries cannot be removed, and the mode cannot go off while a product carries one.
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

	// In use: the class cannot be removed; the refusal changes nothing.
	r := f.authedJSON(t, http.MethodDelete, csClasses+"/"+added.Class.ID, tok, map[string]any{"reason": "cleanup"})
	if r.status != http.StatusConflict || errorCode(t, r.body) != "drug_schedule_class_in_use" {
		t.Fatalf("removing a class in use: %d %s", r.status, r.body)
	}
	if got := csClassList(t, f, tok); len(got.Items) != len(defaults)+1 {
		t.Fatalf("a refused removal changed the list: %v", labelsOf(got))
	}

	// The schedule mode cannot go off while a product carries a class; the SAVE is refused.
	cur := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	off := f.authedJSON(t, http.MethodPatch, csSettings, tok, csPatch(cur.Version, "retiring the field", map[string]map[string]any{"drug_schedule": {"mode": "off"}}))
	if off.status != http.StatusUnprocessableEntity || !slices.Equal(csFieldErrors(t, off)["drug_schedule.mode"], []string{"schedule_in_use"}) {
		t.Fatalf("mode off while a product carries a class: %d %s", off.status, off.body)
	}

	// Clear the product class: the class can then go, and the mode can go off.
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/inventory/products/"+p, tok, map[string]any{"drug_schedule": ""}); r.status != http.StatusNoContent {
		t.Fatalf("clearing the class: %d %s", r.status, r.body)
	}
	removed := csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodDelete, csClasses+"/"+added.Class.ID, tok, map[string]any{"reason": "cleanup"}), http.StatusOK)
	if len(removed.Items) != len(defaults) {
		t.Fatalf("after the removal: %v", labelsOf(removed))
	}
	cur = csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok, csPatch(cur.Version, "retiring the field", map[string]map[string]any{"drug_schedule": {"mode": "off"}})), http.StatusOK)

	// Inventory refuses a class under mode off as well: its replica lags the company
	// settings, so the two refusals are paired.
	syncInventoryReplica(t, f, tok)
	r = f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody("SCH-2", map[string]any{"gst_rate_id": rate, "drug_schedule": removed.Items[0].ID}))
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
	for i, c := range removed.Items {
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invProductBody("SCH-C"+string(rune('A'+i)), map[string]any{"gst_rate_id": rate, "drug_schedule": c.ID})); r.status != http.StatusCreated {
			t.Fatalf("class %q: %d %s", c.Label, r.status, r.body)
		}
	}
}

// The company may remove every default class; the product form then has none to pick.
func TestE2E_DrugSchedule_AnUnusedDefaultClassCanBeRemoved(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "drugsched2").AccessToken
	for _, c := range csClassList(t, f, tok).Items {
		csDecode[csports.ScheduleClassesDto](t, f.authedJSON(t, http.MethodDelete, csClasses+"/"+c.ID, tok, map[string]any{"reason": "this company does not use them"}), http.StatusOK)
	}
	if got := csClassList(t, f, tok); len(got.Items) != 0 {
		t.Fatalf("list after removing every class: %v", labelsOf(got))
	}
	if r := f.authedJSON(t, http.MethodDelete, csClasses+"/00000000-0000-4000-8000-0000000000aa", tok, map[string]any{"reason": "x"}); r.status != http.StatusNotFound {
		t.Fatalf("an unknown class: %d", r.status)
	}
}
