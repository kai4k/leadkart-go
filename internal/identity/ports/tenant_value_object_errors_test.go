package ports_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// A malformed statutory id or contact detail is the caller's mistake: 422
// validation_failed with a per-field entry, never a 500.
func TestTenantMutations_ValueObjectErrorsAre422WithTheField(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	const (
		goodGST = "27AAPFU0939F1ZV"
		goodPAN = "AAPFU0939F"
		goodDL  = "MH-MZ1-123456"
	)
	statutory := func(g, p, d string) string {
		b, _ := json.Marshal(map[string]string{"gst_number": g, "pan_number": p, "drug_licence_number": d}) // arch-test:ignore-err - fixed shape
		return string(b)
	}
	contactWith := func(phone string, addr map[string]string) string {
		b, _ := json.Marshal(map[string]any{"phone": phone, "address": addr}) // arch-test:ignore-err - fixed shape
		return string(b)
	}
	goodAddr := func(over map[string]string) map[string]string {
		m := map[string]string{
			"street": "1 Main Rd", "city": "Pune", "district": "Pune", "state": "Maharashtra", "state_code": "MH", "pincode": "411001",
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	const goodPhone = "+919812345678"
	cases := []struct {
		name, path, body, field, code string
	}{
		{"gst", "statutory", statutory("not-a-gstin", goodPAN, goodDL), "gst_number", "invalid_gstin"},
		{"pan", "statutory", statutory(goodGST, "nope", goodDL), "pan_number", "invalid_pan"},
		{"drug licence", "statutory", statutory(goodGST, goodPAN, "???"), "drug_licence_number", "invalid_drug_licence"},
		{"phone", "admin-contact", contactWith("12345", goodAddr(nil)), "phone", "invalid_phone"},
		{"postal pincode shape", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"pincode": "12"})), "address.pincode", "invalid_pincode"},
		{"postal pincode leading zero", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"pincode": "012345"})), "address.pincode", "invalid_pincode"},
		{"postal pincode blank", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"pincode": ""})), "address.pincode", "required"},
		{"postal street blank", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"street": ""})), "address.street", "required"},
		{"postal street too long", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"street": strings.Repeat("x", 201)})), "address.street", "too_long"},
		{"postal city blank", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"city": ""})), "address.city", "required"},
		{"postal city too long", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"city": strings.Repeat("x", 81)})), "address.city", "too_long"},
		{"postal district too long", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"district": strings.Repeat("x", 81)})), "address.district", "too_long"},
		{"postal state blank", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"state": ""})), "address.state", "required"},
		{"postal state too long", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"state": strings.Repeat("x", 81)})), "address.state", "too_long"},
		{"postal state code too long", "admin-contact", contactWith(goodPhone, goodAddr(map[string]string{"state_code": "KARNT"})), "address.state_code", "too_long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := tenanttest.NewFakeRepository()
			var a app.Application
			a.Commands.UpdateTenantStatutory = command.NewUpdateTenantStatutoryHandler(repo, nil)
			a.Commands.UpdateTenantAdminContact = command.NewUpdateTenantAdminContactHandler(repo, nil)
			rec := azServeApp(t, a, azClaims(p.Tenants.Update), "PATCH", "/api/v1/tenants/"+azTenant+"/"+tc.path, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d %s, want 422", rec.Code, rec.Body.String())
			}
			var er ports.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if er.Error != ports.ErrCodeValidationFailed {
				t.Errorf("code %q, want validation_failed", er.Error)
			}
			if got := er.Errors[tc.field]; len(got) != 1 || got[0] != tc.code {
				t.Errorf("errors[%q] = %v, want [%s] (all: %v)", tc.field, got, tc.code, er.Errors)
			}
			if len(er.Errors) != 1 {
				t.Errorf("errors has %d keys, want exactly the one: %v", len(er.Errors), er.Errors)
			}
			assertNoSentence(t, er.Errors)
		})
	}
}

// assertNoSentence fails when a value in an errors map holds a space: a code is
// one token (snake_case), a sentence cannot be translated or matched by a client.
func assertNoSentence(t *testing.T, errs map[string][]string) {
	t.Helper()
	for field, codes := range errs {
		for _, c := range codes {
			if c == "" || strings.ContainsAny(c, " \t\n") {
				t.Errorf("errors[%q] holds %q: a field error must be a stable code, not a sentence", field, c)
			}
		}
	}
}

// The permission-override conflict answers `revoked: [also_granted]`: a code,
// never a sentence naming the permission.
func TestUserPermissionOverrides_ConflictIsACode(t *testing.T) {
	t.Parallel()
	pm := permission.IdentityPermissions
	body := `{"granted":["` + pm.Roles.Update + `"],"revoked":["` + pm.Roles.Update + `"],"overrides_version":"x"}`
	rec := azServeApp(t, selfTargetApp(t), bodyAdminClaims(pm.Meta.TenantAdmin), "PATCH",
		"/api/v1/users/"+azOther+"/permission-overrides", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s, want 422", rec.Code, rec.Body.String())
	}
	var er ports.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Error != ports.ErrCodeValidationFailed {
		t.Errorf("code %q, want validation_failed", er.Error)
	}
	if got := er.Errors["revoked"]; len(got) != 1 || got[0] != "also_granted" {
		t.Errorf("errors[revoked] = %v, want [also_granted]", got)
	}
	assertNoSentence(t, er.Errors)
}

// The other tenant mutation routes already map their invalid input (it wraps
// tenant.ErrInvalid); pin that none of them falls through to 500.
func TestTenantMutations_OtherInvalidInputIsNever500(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name, method, path, body string
		perm                     string
	}{
		{"settings", "PATCH", "settings", `{"password_policy":{"min_length":1,"require_uppercase":true,"require_lowercase":true,"require_digit":true,"require_symbol":true,"max_failed_attempts":5,"lockout_minutes":15}}`, p.Tenants.UpdateSettings},
		{"display preferences", "PATCH", "display-preferences", `{"locale":"!!","time_zone":"Asia/Kolkata","date_format":"DD/MM/YYYY","currency":"INR"}`, p.Tenants.Update},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := tenanttest.NewFakeRepository()
			var a app.Application
			a.Commands.UpdateTenantSettings = command.NewUpdateTenantSettingsHandler(repo, nil)
			a.Commands.UpdateTenantDisplayPreferences = command.NewUpdateTenantDisplayPreferencesHandler(repo, nil)
			rec := azServeApp(t, a, azClaims(tc.perm), tc.method, "/api/v1/tenants/"+azTenant+"/"+tc.path, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d %s, want 422", rec.Code, rec.Body.String())
			}
		})
	}
}
