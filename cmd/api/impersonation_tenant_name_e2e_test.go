//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// An impersonation session names the company it acts in: the web prints
// "Impersonating <company>" on every page, and after a reload (or when another tab
// opened a different session) it must read that name from the API.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// jsonObject decodes a body into a loose map so the test sees exactly what is on the wire.
func jsonObject(t *testing.T, r httpResp) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return m
}

func (f e2eFixture) renameTenant(t *testing.T, tn registeredTenant, display string) {
	t.Helper()
	r := f.authedJSON(t, http.MethodPatch, "/api/v1/tenants/"+tn.TenantID+"/profile", tn.AccessToken,
		ports.UpdateTenantProfileRequest{LegalName: display + " Pvt Ltd", DisplayName: display})
	if r.status != http.StatusNoContent {
		t.Fatalf("rename tenant to %q: %d %s", display, r.status, r.body)
	}
}

func TestE2E_Impersonation_SessionNamesItsCompany_CurrentNameNeverAnothers(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "namea")
	tenantB := f.registerAndLogin(t, "nameb")
	f.renameTenant(t, tenantA, "Alpha Remedies")
	f.renameTenant(t, tenantB, "Bravo Labs")

	opTok, sessA := f.openImpersonation(t, tenantA.TenantID)
	// A second session by the same operator, against the other tenant.
	respB := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", opTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tenantB.TenantID, Reason: "ticket 4712 second session", DurationMinutes: 30})
	if respB.status != http.StatusCreated {
		t.Fatalf("create second session: %d %s", respB.status, respB.body)
	}
	var sessB ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(respB.body, &sessB); err != nil {
		t.Fatal(err)
	}

	// create -> the name is present.
	if sessA.TenantDisplayName != "Alpha Remedies" || sessB.TenantDisplayName != "Bravo Labs" {
		t.Errorf("create responses tenant_display_name = %q / %q, want Alpha Remedies / Bravo Labs", sessA.TenantDisplayName, sessB.TenantDisplayName)
	}

	// list -> each session carries its own company's name.
	names := func() map[string]string {
		t.Helper()
		r := f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", opTok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("list sessions: %d %s", r.status, r.body)
		}
		out := map[string]string{}
		for _, s := range jsonObject(t, r)["sessions"].([]any) {
			m := s.(map[string]any)
			n, _ := m["tenant_display_name"].(string)
			out[m["session_id"].(string)] = n
		}
		return out
	}
	if got := names(); got[sessA.SessionID] != "Alpha Remedies" || got[sessB.SessionID] != "Bravo Labs" {
		t.Errorf("list names = %v, want A=Alpha Remedies B=Bravo Labs", got)
	}

	// An impersonation token reads its OWN session's name (and nothing of another's).
	ctx := func(tok string) (id, name string) {
		t.Helper()
		r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("capabilities: %d %s", r.status, r.body)
		}
		imp, ok := jsonObject(t, r)["impersonation"].(map[string]any)
		if !ok {
			t.Fatalf("capabilities under a session carries no impersonation object: %s", r.body)
		}
		id, _ = imp["session_id"].(string)
		name, _ = imp["tenant_display_name"].(string)
		return id, name
	}
	if id, name := ctx(sessA.AccessToken); id != sessA.SessionID || name != "Alpha Remedies" {
		t.Errorf("session A token reads (%q, %q)", id, name)
	}
	if id, name := ctx(sessB.AccessToken); id != sessB.SessionID || name != "Bravo Labs" {
		t.Errorf("session B token reads (%q, %q)", id, name)
	}
	// Listing sessions is a platform route: the impersonation token cannot read the list.
	f.requireImpersonationRefused(t, "an impersonation token lists sessions", http.MethodGet, "/api/v1/platform/impersonation/sessions", sessA.AccessToken, nil)

	// A rename shows on the next read, on both surfaces.
	f.renameTenant(t, tenantA, "Alpha Renamed")
	if got := names(); got[sessA.SessionID] != "Alpha Renamed" || got[sessB.SessionID] != "Bravo Labs" {
		t.Errorf("list names after rename = %v", got)
	}
	if _, name := ctx(sessA.AccessToken); name != "Alpha Renamed" {
		t.Errorf("capabilities name after rename = %q, want Alpha Renamed", name)
	}

	// A normal (non-impersonation) token has no impersonation object.
	r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", tenantA.AccessToken, nil)
	if _, has := jsonObject(t, r)["impersonation"]; has {
		t.Errorf("capabilities of an ordinary token carries an impersonation object: %s", r.body)
	}
}
