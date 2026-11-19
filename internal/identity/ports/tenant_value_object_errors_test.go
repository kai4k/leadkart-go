package ports_test

import (
	"encoding/json"
	"net/http"
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
	contact := func(phone, pincode string) string {
		b, _ := json.Marshal(map[string]any{"phone": phone, "address": map[string]string{ // arch-test:ignore-err - fixed shape
			"street": "1 Main Rd", "city": "Pune", "district": "Pune", "state": "Maharashtra", "state_code": "MH", "pincode": pincode,
		}})
		return string(b)
	}
	cases := []struct {
		name, path, body, field string
	}{
		{"gst", "statutory", statutory("not-a-gstin", goodPAN, goodDL), "gst_number"},
		{"pan", "statutory", statutory(goodGST, "nope", goodDL), "pan_number"},
		{"drug licence", "statutory", statutory(goodGST, goodPAN, "???"), "drug_licence_number"},
		{"phone", "admin-contact", contact("12345", "411001"), "phone"},
		{"address", "admin-contact", contact("+919812345678", "12"), "address"},
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
			if er.Error != ports.ErrCodeValidationFailed || len(er.Errors[tc.field]) == 0 {
				t.Errorf("body %+v, want validation_failed with an entry for %q", er, tc.field)
			}
		})
	}
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
