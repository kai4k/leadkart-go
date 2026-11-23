//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// End-to-end tests for the company commerce settings routes (ADR 0070): the
// policy read, the versioned patch (stale save is 409), field refusals with
// stable codes, the Owner / Admin tier rule, the GST rate list and the history.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	csports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/identitytest"
)

const (
	csSettings = "/api/v1/company/commerce-settings"
	csHistory  = "/api/v1/company/commerce-settings/history"
	csRates    = "/api/v1/company/gst-rates"
)

// csPatch is the PATCH body: changes keyed group then setting.
func csPatch(expected int64, reason string, changes map[string]map[string]any) map[string]any {
	return map[string]any{"expected_version": expected, "reason": reason, "changes": changes}
}

func csDecode[T any](t *testing.T, r httpResp, wantStatus int) T {
	t.Helper()
	var out T
	if r.status != wantStatus {
		t.Fatalf("status %d body %s, want %d", r.status, r.body, wantStatus)
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return out
}

// csFieldErrors returns the errors{field:[code]} map of a refusal.
func csFieldErrors(t *testing.T, r httpResp) map[string][]string {
	t.Helper()
	var e struct {
		Errors map[string][]string `json:"errors"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return e.Errors
}

// csTokenAt mints a token for the registered tenant's admin person at the given
// rank holding exactly perms, so a rank below the Administrator tier can be
// shown to hold a company_settings grant that is inert.
func csTokenAt(t *testing.T, f e2eFixture, reg registeredTenant, rank int, perms ...string) string {
	t.Helper()
	tok, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID: reg.PersonID, TenantID: reg.TenantID, TenantSlug: reg.Slug, MembershipID: reg.MembershipID,
		SecurityStamp: identitytest.GetSecurityStamp(t, f.Pool, reg.PersonID), Permissions: perms, Rank: &rank,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return tok
}

func TestE2E_CompanySettings_ReadsDefaultsPatchesAndKeepsHistory(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "cs-flow")
	tok := reg.AccessToken

	// A company with nothing stored reads the defaults at version 0; GST starts empty.
	got := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	if got.Version != 0 || got.Policy.Expiry.StopMonthsBefore != 1 || len(got.Policy.Tax.GSTRates) != 0 {
		t.Fatalf("defaults = v%d expiry %d rates %d", got.Version, got.Policy.Expiry.StopMonthsBefore, len(got.Policy.Tax.GSTRates))
	}
	if got.Policy.DrugSchedule.Mode != commercepolicy.DrugScheduleOptional {
		t.Fatalf("drug schedule default = %q", got.Policy.DrugSchedule.Mode)
	}
	var meta *commercepolicy.SettingInfo
	for i := range got.Options {
		if got.Options[i].Key == "expiry.stop_months_before" {
			meta = &got.Options[i]
		}
	}
	if meta == nil || meta.Permission != commercepolicy.PermissionManage || string(meta.Default) != "1" {
		t.Fatalf("option metadata for expiry.stop_months_before = %+v", meta)
	}

	// Patch: the response is the updated policy at the next version.
	patched := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(got.Version, "", map[string]map[string]any{"expiry": {"stop_months_before": 3}})), http.StatusOK)
	if patched.Version != 1 || patched.Policy.Expiry.StopMonthsBefore != 3 {
		t.Fatalf("patched = v%d expiry %d", patched.Version, patched.Policy.Expiry.StopMonthsBefore)
	}

	// A stale save is a 409 that names the version to reload, and changes nothing.
	stale := f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(0, "", map[string]map[string]any{"expiry": {"stop_months_before": 6}}))
	if stale.status != http.StatusConflict || errorCode(t, stale.body) != "settings_version_conflict" {
		t.Fatalf("stale save: %d %s", stale.status, stale.body)
	}
	var conflict csports.ErrorResponse
	if err := json.Unmarshal(stale.body, &conflict); err != nil || conflict.CurrentVersion == nil || *conflict.CurrentVersion != 1 {
		t.Fatalf("conflict body %s: %v", stale.body, err)
	}
	again := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	if again.Version != 1 || again.Policy.Expiry.StopMonthsBefore != 3 {
		t.Fatalf("a stale save changed the policy: v%d expiry %d", again.Version, again.Policy.Expiry.StopMonthsBefore)
	}

	// A patch equal to the current policy does not move the version.
	same := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(1, "", map[string]map[string]any{"expiry": {"stop_months_before": 3}})), http.StatusOK)
	if same.Version != 1 {
		t.Fatalf("a no-op patch moved the version to %d", same.Version)
	}

	// A tax setting needs a reason; with one it is stored and recorded.
	noReason := f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(1, "", map[string]map[string]any{"invoice": {"round_off": "nearest_rupee"}}))
	if noReason.status != http.StatusUnprocessableEntity || !slices.Equal(csFieldErrors(t, noReason)["reason"], []string{"required"}) {
		t.Fatalf("a tax change without a reason: %d %s", noReason.status, noReason.body)
	}
	taxed := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(1, "per CA letter 12 Nov", map[string]map[string]any{"invoice": {"round_off": "nearest_rupee"}})), http.StatusOK)
	if taxed.Version != 2 || taxed.Policy.Invoice.RoundOff != "nearest_rupee" {
		t.Fatalf("taxed = v%d %q", taxed.Version, taxed.Policy.Invoice.RoundOff)
	}

	// History: newest first, the whole policy, the keys, the actor and the reason.
	hist := csDecode[csports.ListHistoryResponse](t, f.authedJSON(t, http.MethodGet, csHistory, tok, nil), http.StatusOK)
	if len(hist.Items) != 2 || hist.Items[0].Version != 2 || hist.Items[1].Version != 1 {
		t.Fatalf("history = %+v", hist.Items)
	}
	if hist.Items[0].Reason != "per CA letter 12 Nov" || !slices.Equal(hist.Items[0].ChangedKeys, []string{"invoice.round_off"}) ||
		hist.Items[0].ActorMembershipID != reg.MembershipID || hist.Items[1].Policy.Expiry.StopMonthsBefore != 3 {
		t.Fatalf("history entry = %+v", hist.Items[0])
	}
	page := csDecode[csports.ListHistoryResponse](t, f.authedJSON(t, http.MethodGet, csHistory+"?page_size=1", tok, nil), http.StatusOK)
	if len(page.Items) != 1 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	next := csDecode[csports.ListHistoryResponse](t, f.authedJSON(t, http.MethodGet, csHistory+"?page_size=1&cursor="+page.NextCursor, tok, nil), http.StatusOK)
	if len(next.Items) != 1 || next.Items[0].Version != 1 || next.HasMore {
		t.Fatalf("second page = %+v", next)
	}
	if r := f.authedJSON(t, http.MethodGet, csHistory+"?cursor=not-a-cursor", tok, nil); r.status != http.StatusBadRequest {
		t.Fatalf("a bad cursor: %d %s", r.status, r.body)
	}
}

func TestE2E_CompanySettings_RefusalsNameTheSettingWithStableCodes(t *testing.T) {
	f := newE2EFixture(t)
	tok := f.registerAndLogin(t, "cs-refuse").AccessToken
	patch := func(v int64, reason string, changes map[string]map[string]any) httpResp {
		return f.authedJSON(t, http.MethodPatch, csSettings, tok, csPatch(v, reason, changes))
	}
	cases := []struct {
		name    string
		changes map[string]map[string]any
		field   string
		code    string
	}{
		{"expiry below the legal limit", map[string]map[string]any{"expiry": {"stop_months_before": -1}}, "expiry.stop_months_before", "out_of_range"},
		{"expiry not a number", map[string]map[string]any{"expiry": {"stop_months_before": "soon"}}, "expiry.stop_months_before", "invalid_type"},
		{"not an option", map[string]map[string]any{"eway": {"enforcement": "maybe"}}, "eway.enforcement", "not_an_option"},
		{"too many warning days", map[string]map[string]any{"expiry": {"warning_days": []int{1, 2, 3, 4, 5}}}, "expiry.warning_days", "too_many"},
		{"unknown setting", map[string]map[string]any{"expiry": {"grace_days": 5}}, "expiry.grace_days", "unknown_setting"},
		{"gst rates are not patched", map[string]map[string]any{"tax": {"gst_rates": []any{}}}, "tax.gst_rates", "managed_elsewhere"},
		{"percent advance needs a value", map[string]map[string]any{"payment": {"advance_mode": "percent"}}, "payment.advance_value", "out_of_range"},
		{"default outside the allowed set", map[string]map[string]any{"fulfilment": {"shortage_allowed": []string{"refuse"}}}, "fulfilment.shortage_default", "not_in_allowed"},
	}
	for _, tc := range cases {
		r := patch(0, "because", tc.changes)
		if r.status != http.StatusUnprocessableEntity || errorCode(t, r.body) != "validation_failed" {
			t.Errorf("%s: %d %s", tc.name, r.status, r.body)
			continue
		}
		if got := csFieldErrors(t, r)[tc.field]; len(got) == 0 || got[0] != tc.code {
			t.Errorf("%s: errors = %v, want %s: [%s]", tc.name, csFieldErrors(t, r), tc.field, tc.code)
		}
	}
	missing := f.authedJSON(t, http.MethodPatch, csSettings, tok, map[string]any{"changes": map[string]any{}})
	errs := csFieldErrors(t, missing)
	if missing.status != http.StatusUnprocessableEntity || errs["expected_version"][0] != "required" || errs["changes"][0] != "required" {
		t.Errorf("missing expected_version and changes: %d %s", missing.status, missing.body)
	}
	if r := f.authedJSON(t, http.MethodPatch, csSettings, tok, map[string]any{"expected_version": 0, "changes": map[string]any{"expiry": 5}}); r.status != http.StatusBadRequest {
		t.Errorf("a group that is not an object: %d %s", r.status, r.body)
	}
	// Nothing above changed the policy.
	got := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	if got.Version != 0 {
		t.Errorf("refusals moved the version to %d", got.Version)
	}
}

// Owner ruling 2026-11-23: only the company's Owner / Admin tier changes the
// settings. A grant below the tier is inert; the permission split between
// manage and tax.manage is enforced per changed key.
func TestE2E_CompanySettings_OnlyTheAdminTierChangesThem(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "cs-tier")
	p := permission.IdentityPermissions.CompanySettings
	credit := csPatch(0, "r", map[string]map[string]any{"credit": {"enabled": true}})

	manager := csTokenAt(t, f, reg, 20, p.Read, p.Manage, p.TaxManage)
	if r := f.authedJSON(t, http.MethodGet, csSettings, manager, nil); r.status != http.StatusOK {
		t.Errorf("a manager may read the policy: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPatch, csSettings, manager, credit); r.status != http.StatusForbidden {
		t.Errorf("a manager holding the write grants is refused: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, csRates, manager, map[string]any{"rate_bps": 500, "label": "GST 5%", "confirmed_by": "A", "confirmed_on": "2026-01-01", "reason": "r"}); r.status != http.StatusForbidden {
		t.Errorf("a manager may not add a rate: %d %s", r.status, r.body)
	}
	noGrant := csTokenAt(t, f, reg, 50, permission.IdentityPermissions.Notifications.Inbox.Read)
	if r := f.authedJSON(t, http.MethodGet, csSettings, noGrant, nil); r.status != http.StatusForbidden {
		t.Errorf("a member without company_settings.read: %d %s", r.status, r.body)
	}

	// An admin-tier role holding manage only: non-tax keys pass, tax keys are refused by name.
	manageOnly := csTokenAt(t, f, reg, 10, p.Read, p.Manage)
	if r := f.authedJSON(t, http.MethodPatch, csSettings, manageOnly, credit); r.status != http.StatusOK {
		t.Errorf("manage-only admin changing credit.enabled: %d %s", r.status, r.body)
	}
	tax := f.authedJSON(t, http.MethodPatch, csSettings, manageOnly,
		csPatch(1, "r", map[string]map[string]any{"eway": {"enforcement": "block"}}))
	if tax.status != http.StatusForbidden || errorCode(t, tax.body) != "forbidden" {
		t.Errorf("manage-only admin changing a tax key: %d %s", tax.status, tax.body)
	}
	taxOnly := csTokenAt(t, f, reg, 10, p.Read, p.TaxManage)
	other := f.authedJSON(t, http.MethodPatch, csSettings, taxOnly,
		csPatch(1, "", map[string]map[string]any{"credit": {"enabled": false}}))
	if other.status != http.StatusForbidden {
		t.Errorf("tax-only admin changing a non-tax key: %d %s", other.status, other.body)
	}
}

func TestE2E_CompanySettings_GSTRateListStartsEmptyAndDeactivatesNeverDeletes(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "cs-gst")
	tok := reg.AccessToken
	body := func(rate int, label string) map[string]any {
		return map[string]any{"rate_bps": rate, "label": label, "confirmed_by": "A. Mehta (CA)", "confirmed_on": "2026-10-30", "reason": "per CA"}
	}

	empty := csDecode[csports.ListGSTRatesResponse](t, f.authedJSON(t, http.MethodGet, csRates, tok, nil), http.StatusOK)
	if len(empty.Items) != 0 || empty.PolicyVersion != 0 {
		t.Fatalf("a new company has no rate: %+v", empty)
	}
	five := csDecode[csports.GSTRateChangedDto](t, f.authedJSON(t, http.MethodPost, csRates, tok, body(500, "GST 5%")), http.StatusCreated)
	if five.ID == "" || !five.Active || five.PolicyVersion != 1 || five.ConfirmedBy != "A. Mehta (CA)" {
		t.Fatalf("created = %+v", five)
	}
	csDecode[csports.GSTRateChangedDto](t, f.authedJSON(t, http.MethodPost, csRates, tok, body(1200, "GST 12%")), http.StatusCreated)

	dup := f.authedJSON(t, http.MethodPost, csRates, tok, body(500, "gst 5%"))
	if dup.status != http.StatusUnprocessableEntity || !slices.Equal(csFieldErrors(t, dup)["label"], []string{"duplicate"}) {
		t.Fatalf("duplicate rate: %d %s", dup.status, dup.body)
	}
	bad := f.authedJSON(t, http.MethodPost, csRates, tok, map[string]any{"rate_bps": 20000, "label": "", "confirmed_by": "A", "confirmed_on": "tomorrow"})
	errs := csFieldErrors(t, bad)
	if bad.status != http.StatusUnprocessableEntity || errs["rate_bps"][0] != "out_of_range" || errs["label"][0] != "required" ||
		errs["confirmed_on"][0] != "invalid_format" || errs["reason"][0] != "required" {
		t.Fatalf("field refusals: %d %s", bad.status, bad.body)
	}
	if r := f.authedJSON(t, http.MethodPost, csRates, tok, map[string]any{"label": "x"}); r.status != http.StatusUnprocessableEntity || csFieldErrors(t, r)["rate_bps"][0] != "required" {
		t.Fatalf("missing rate: %d %s", r.status, r.body)
	}

	off := csDecode[csports.GSTRateChangedDto](t, f.authedJSON(t, http.MethodPatch, csRates+"/"+five.ID, tok,
		map[string]any{"active": false, "reason": "rate retired"}), http.StatusOK)
	if off.Active || off.PolicyVersion != 3 {
		t.Fatalf("deactivated = %+v", off)
	}
	all := csDecode[csports.ListGSTRatesResponse](t, f.authedJSON(t, http.MethodGet, csRates, tok, nil), http.StatusOK)
	active := csDecode[csports.ListGSTRatesResponse](t, f.authedJSON(t, http.MethodGet, csRates+"?active=true", tok, nil), http.StatusOK)
	if len(all.Items) != 2 || all.Items[0].RateBps != 500 || len(active.Items) != 1 || active.Items[0].RateBps != 1200 {
		t.Fatalf("all=%+v active=%+v", all.Items, active.Items)
	}
	if r := f.authedJSON(t, http.MethodPatch, csRates+"/"+uuid.NewString(), tok, map[string]any{"active": false, "reason": "r"}); r.status != http.StatusNotFound {
		t.Fatalf("unknown rate: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPatch, csRates+"/not-a-uuid", tok, map[string]any{"active": false, "reason": "r"}); r.status != http.StatusBadRequest {
		t.Fatalf("bad rate id: %d %s", r.status, r.body)
	}
	// The rate list is part of the policy: the settings read shows the same list.
	pol := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	if len(pol.Policy.Tax.GSTRates) != 2 || pol.Version != 3 {
		t.Fatalf("policy rates = %+v v%d", pol.Policy.Tax.GSTRates, pol.Version)
	}

	// A catalogue reader may pick a rate but not add one.
	p := permission.IdentityPermissions
	picker := csTokenAt(t, f, reg, 50, p.Inventory.Catalog.Read)
	if r := f.authedJSON(t, http.MethodGet, csRates+"?active=true", picker, nil); r.status != http.StatusOK {
		t.Errorf("catalogue reader reading the rate list: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, csRates, picker, body(1800, "GST 18%")); r.status != http.StatusForbidden {
		t.Errorf("catalogue reader adding a rate: %d %s", r.status, r.body)
	}
}

func TestE2E_CompanySettings_TenantsDoNotSeeEachOther(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "cs-iso-a")
	b := f.registerAndLogin(t, "cs-iso-b")
	csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, a.AccessToken,
		csPatch(0, "", map[string]map[string]any{"expiry": {"stop_months_before": 5}})), http.StatusOK)
	csDecode[csports.GSTRateChangedDto](t, f.authedJSON(t, http.MethodPost, csRates, a.AccessToken,
		map[string]any{"rate_bps": 500, "label": "GST 5%", "confirmed_by": "A", "confirmed_on": "2026-10-30", "reason": "r"}), http.StatusCreated)

	got := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, b.AccessToken, nil), http.StatusOK)
	rates := csDecode[csports.ListGSTRatesResponse](t, f.authedJSON(t, http.MethodGet, csRates, b.AccessToken, nil), http.StatusOK)
	hist := csDecode[csports.ListHistoryResponse](t, f.authedJSON(t, http.MethodGet, csHistory, b.AccessToken, nil), http.StatusOK)
	if got.Version != 0 || got.Policy.Expiry.StopMonthsBefore != 1 || len(rates.Items) != 0 || len(hist.Items) != 0 {
		t.Fatalf("tenant B saw tenant A: v%d expiry %d rates %d history %d", got.Version, got.Policy.Expiry.StopMonthsBefore, len(rates.Items), len(hist.Items))
	}
}
