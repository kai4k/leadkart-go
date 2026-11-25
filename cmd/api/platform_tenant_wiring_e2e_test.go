//go:build integration

// arch-test:no-timeout-needed — one test over newE2EFixture, which bounds its own startup; per-request
// HTTP uses t.Context().

package main

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// G2q m1: pins the newServer wiring of authn.PlatformTenant. The resolver reads the real platform
// tenant's id under the application role, and that id reaches the actor over real HTTP: a token whose
// OWN tenant is not the platform tenant (the fixture's synthetic operator; used here only to make the
// wiring observable) acting in the platform tenant via X-Tenant-Id gets the platform rule, so it cannot
// lift a platform staff member's managed revoke. Without the middleware in newServer the platform
// tenant would be treated as a company and the lift would succeed (204).
func TestPlatformTenantWiring_ResolverIdReachesTheActorOverHTTP(t *testing.T) {
	f := newE2EFixture(t)
	resolve := ports.NewPlatformTenantResolver(slog.New(slog.NewTextHandler(io.Discard, nil)), f.app)
	if got := resolve(t.Context()); got != "" {
		t.Fatalf("before the platform tenant exists: %q", got)
	}
	plat := f.registerPlatformTenant(t)
	if got := resolve(t.Context()); string(got) != plat.TenantID {
		t.Fatalf("resolver id = %q, want the registered platform tenant %q", got, plat.TenantID)
	}

	target := f.createMember(t, plat, "plat-target")
	p := permission.IdentityPermissions
	path := "/api/v1/users/" + target.membershipID + "/permission-overrides"
	plant := f.authedJSON(t, http.MethodPatch, path, plat.AccessToken,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{p.Roles.Update}})
	if plant.status >= 300 {
		t.Fatalf("platform owner revokes %s on a staff member: %d %s", p.Roles.Update, plant.status, plant.body)
	}

	op := f.mintPlatformTokenWith(t, nil, false) // synthetic own tenant, no role-write fact
	r := f.doWithHeaders(t, http.MethodPatch, path, op,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}, scopeHdr(plat.TenantID))
	if r.status != http.StatusForbidden || errCodeOf(r) != "grant_exceeds_authority" {
		t.Errorf("lift of a managed revoke in the platform tenant via X-Tenant-Id: %d %s, want 403 grant_exceeds_authority", r.status, r.body)
	}
}
