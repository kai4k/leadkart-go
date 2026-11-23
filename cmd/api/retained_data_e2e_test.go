//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).
//
// arch-test:raw-sql-justified — the stored status of a tenant and the persons table
// have no HTTP surface that shows "the row is still there".

// ADR 0071 (owner decision 2026-11-23: "We never delete any data"): the routes that
// anonymised a person or deleted a company are gone, and the lifecycle that remains
// (a person active or inactive, a company active or suspended) still works end to end,
// with every refusal of an inactive account exactly as before.
package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_RemovedErasureRoutes_AreGone(t *testing.T) {
	f := newE2EFixture(t)
	v := f.registerAndLogin(t, "gone")
	su := f.mintPlatformTokenWith(t, nil, true)
	for _, rt := range []struct{ what, method, path string }{
		{"anonymise a user", http.MethodPost, "/api/v1/users/" + v.MembershipID + "/anonymise"},
		{"anonymise a person", http.MethodPost, "/api/v1/platform/persons/" + v.PersonID + "/anonymise"},
		{"mark a company for deletion", http.MethodPost, "/api/v1/tenants/" + v.TenantID + "/mark-for-deletion"},
		{"restore a company", http.MethodPost, "/api/v1/tenants/" + v.TenantID + "/restore"},
		{"delete a company", http.MethodDelete, "/api/v1/platform/tenants/" + v.TenantID},
	} {
		r := f.authedJSON(t, rt.method, rt.path, su, map[string]string{"reason": "must not exist"})
		if r.status != http.StatusNotFound && r.status != http.StatusMethodNotAllowed {
			t.Errorf("%s: %s %s = %d %s, want the route to be gone (404 or 405)", rt.what, rt.method, rt.path, r.status, r.body)
		}
	}
	// Nothing happened to the person or the company.
	if got := f.tenantStatusOf(t, v.TenantID); got != "active" {
		t.Errorf("tenant status %q after the removed routes were called, want active", got)
	}
	var active bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT is_active FROM identity.persons WHERE id = $1`, v.PersonID).Scan(&active); err != nil || !active {
		t.Errorf("person is_active = %v (%v), want true", active, err)
	}
}

func TestE2E_PersonAndCompanyLifecycle_OnlyActiveInactiveAndActiveSuspended(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "lifeonly")
	m := f.createMember(t, owner, "lifemember")

	// A person is switched inactive and back; the row is never removed.
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", owner.AccessToken, ports.DeactivateUserRequest{Reason: "left the team"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate: %d %s", r.status, r.body)
	}
	if got := f.getUser(t, owner.AccessToken, m.membershipID); got.Status != "inactive" {
		t.Fatalf("membership status %q after deactivate, want inactive (the row is kept)", got.Status)
	}
	// The refusal is exactly an unknown email's: same status and body.
	wrong := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "d"})
	unknown := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: "nobody-" + ids.NewV7().String()[24:] + "@e2e.test", Password: m.password, DeviceLabel: "d"})
	if wrong.status != http.StatusUnauthorized || surfaceOf(wrong) != surfaceOf(unknown) {
		t.Fatalf("login of a deactivated member %+v differs from an unknown email %+v", surfaceOf(wrong), surfaceOf(unknown))
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/reactivate", owner.AccessToken, nil); r.status != http.StatusNoContent {
		t.Fatalf("reactivate: %d %s", r.status, r.body)
	}
	if lr := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "back"}); lr.status != http.StatusOK {
		t.Fatalf("login after reactivate: %d %s, want 200", lr.status, lr.body)
	}

	// A company is suspended and activated again; its status is never anything else.
	f.suspendTenantHTTP(t, owner.TenantID)
	if got := f.tenantStatusOf(t, owner.TenantID); got != "suspended" {
		t.Fatalf("tenant status %q after suspend, want suspended", got)
	}
	f.activateTenantHTTP(t, owner.TenantID)
	if got := f.tenantStatusOf(t, owner.TenantID); got != "active" {
		t.Fatalf("tenant status %q after activate, want active", got)
	}

	// The database refuses any other company status.
	if _, err := f.Pool.Exec(t.Context(), `UPDATE identity.tenants SET status = 'pending_deletion' WHERE id = $1`, owner.TenantID); err == nil {
		t.Error("the database accepted the status pending_deletion; the status check must refuse it")
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE identity.tenants SET status = 'deleted' WHERE id = $1`, owner.TenantID); err == nil {
		t.Error("the database accepted the status deleted; the status check must refuse it")
	}
}
