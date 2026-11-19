//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// Full-replace request bodies. These endpoints write exactly what they receive,
// so a body that leaves a field out would silently CLEAR the stored value. The
// spec marks every field required and the handlers agree: an omitted field is a
// 400 invalid_body naming it, and the stored value is unchanged.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_FullReplaceBodies_OmittedFieldIsRejectedAndNothingChanges(t *testing.T) {
	f := newE2EFixture(t)
	v := f.registerAndLogin(t, "fullrep")
	tok := v.AccessToken
	tenantPath := "/api/v1/tenants/" + v.TenantID
	memberID, _ := f.createMemberAndLogin(t, v, "fullrepmem") // overrides cannot target the caller

	address := map[string]any{"street": "12 MG Road", "city": "Bengaluru", "district": "Bengaluru Urban", "state": "Karnataka", "state_code": "KA", "pincode": "560001"}
	cases := []struct {
		name    string
		method  string
		path    string
		full    map[string]any
		omit    string // key to leave out
		wantKey string // key reported in `errors`
	}{
		{"tenant profile", http.MethodPatch, tenantPath + "/profile",
			map[string]any{"legal_name": "Fullrep Legal Pvt Ltd", "display_name": "Fullrep Display"}, "display_name", "display_name"},
		{"tenant statutory", http.MethodPatch, tenantPath + "/statutory",
			map[string]any{"gst_number": "29ABCPE1234F1Z5", "pan_number": "ABCPE1234F", "drug_licence_number": "KA-W-22B-12345"}, "pan_number", "pan_number"},
		{"tenant admin contact (phone)", http.MethodPatch, tenantPath + "/admin-contact",
			map[string]any{"phone": "+919876543210", "address": address}, "phone", "phone"},
		{"tenant admin contact (address)", http.MethodPatch, tenantPath + "/admin-contact",
			map[string]any{"phone": "+919876543210", "address": address}, "address", "address"},
		{"tenant display preferences", http.MethodPatch, tenantPath + "/display-preferences",
			map[string]any{"locale": "en-IN", "time_zone": "Asia/Kolkata", "date_format": "DD/MM/YYYY", "currency": "INR"}, "currency", "currency"},
		{"tenant settings", http.MethodPatch, tenantPath + "/settings",
			map[string]any{"password_policy": map[string]any{"min_length": 14, "require_uppercase": true, "require_lowercase": true, "require_digit": true, "require_symbol": true, "max_failed_attempts": 5, "lockout_minutes": 30}}, "password_policy", "password_policy"},
		{"user profile", http.MethodPatch, "/api/v1/users/" + v.MembershipID + "/profile",
			map[string]any{"designation": "Owner", "department": "Management", "status_message": "in the office"}, "department", "department"},
		{"user permission overrides", http.MethodPatch, "/api/v1/users/" + memberID + "/permission-overrides",
			map[string]any{"granted": []string{"identity.users.view"}, "revoked": []string{}}, "revoked", "revoked"},
	}
	for _, tc := range cases {
		// The full body is accepted and stored.
		if r := f.authedJSON(t, tc.method, tc.path, tok, tc.full); r.status != http.StatusNoContent {
			t.Errorf("%s: full body: %d %s, want 204", tc.name, r.status, r.body)
			continue
		}
		before := f.readBack(t, tok, tenantPath, v.MembershipID, memberID)

		partial := map[string]any{}
		for k, val := range tc.full {
			if k != tc.omit {
				partial[k] = val
			}
		}
		r := f.authedJSON(t, tc.method, tc.path, tok, partial)
		if r.status != http.StatusBadRequest || decodeErrorCode(t, r.body) != "invalid_body" {
			t.Errorf("%s: body without %q: %d %s, want 400 invalid_body", tc.name, tc.omit, r.status, r.body)
		} else {
			var e ports.ErrorResponse
			if err := json.Unmarshal(r.body, &e); err != nil || len(e.Errors[tc.wantKey]) == 0 {
				t.Errorf("%s: 400 body does not name %q in `errors`: %s", tc.name, tc.wantKey, r.body)
			}
		}
		if after := f.readBack(t, tok, tenantPath, v.MembershipID, memberID); after != before {
			t.Errorf("%s: the rejected body changed stored data:\n before %s\n after  %s", tc.name, before, after)
		}
	}
}

// readBack serialises everything the endpoints above write.
func (f e2eFixture) readBack(t *testing.T, tok, tenantPath string, membershipIDs ...string) string {
	t.Helper()
	tr := f.authedJSON(t, http.MethodGet, tenantPath, tok, nil)
	if tr.status != http.StatusOK {
		t.Fatalf("read back tenant: %d %s", tr.status, tr.body)
	}
	out := string(tr.body)
	for _, id := range membershipIDs {
		ur := f.authedJSON(t, http.MethodGet, "/api/v1/users/"+id, tok, nil)
		if ur.status != http.StatusOK {
			t.Fatalf("read back user %s: %d %s", id, ur.status, ur.body)
		}
		out += "\n" + string(ur.body)
	}
	return out
}
