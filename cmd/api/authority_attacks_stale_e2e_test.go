//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureWith → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).
//
// arch-test:no-synctest — the cascade crosses the real forwarder, router and
// Postgres; a virtual clock cannot order them.

// Attack suite, goal 7: keep using a token after the facts behind it changed. On
// the production stack (stamp cascade wired) every kind of authority change must
// end the affected member's old token on the very next request, whether the
// token was used (cache warm) or never used (cache cold), on an authority command,
// an identity read and another module's route alike.

package main

import (
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// spawn makes a fresh member of the world's tenant with a role and the identity
// route permissions, and logs it in. The token is untouched (cache cold).
func (w *attackWorld) spawn(t *testing.T, prefix, roleID string) (createdMember, string) {
	t.Helper()
	m := w.f.createMember(t, w.owner, prefix)
	w.f.assignRole(t, w.owner.AccessToken, m.membershipID, roleID)
	w.f.grantOverrides(t, w.owner.AccessToken, m.membershipID, atkRoutePerms(), nil)
	return m, w.f.loginToken(t, m.email, m.password)
}

// TestAttack_Stale_EveryAuthorityChangeEndsTheOldToken is goal 7.
func TestAttack_Stale_EveryAuthorityChangeEndsTheOldToken(t *testing.T) {
	w := newAttackWorldWith(t, true)
	f := w.f
	p := permission.IdentityPermissions
	salesMgr := w.roles[role.SystemRoles.Tenant.SalesManager]
	execRole := w.roles[role.SystemRoles.Tenant.SalesExecutive]
	jr := f.createMember(t, w.owner, "stalejr") // a junior whose overrides are already empty: replacing them with nothing is a no-op
	f.assignRole(t, w.owner.AccessToken, jr.membershipID, execRole)

	type change struct {
		name   string
		roleID func(t *testing.T) string // the role the member is spawned with
		apply  func(t *testing.T, m createdMember, ownRole string)
	}
	ownerTok := w.owner.AccessToken
	customWithInventory := func(t *testing.T) string {
		id := f.customRole(t, ownerTok, "Stale Role "+ids.NewV7().String()[24:], 40)
		if r := f.grantRolePerm(t, ownerTok, id, p.Inventory.Catalog.Read); r.status != http.StatusNoContent {
			t.Fatalf("grant inventory.catalog.read: %d %s", r.status, r.body)
		}
		return id
	}
	sales := func(*testing.T) string { return salesMgr }
	changes := []change{
		{"a role is revoked", sales, func(t *testing.T, m createdMember, _ string) {
			if r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+m.membershipID+"/roles/"+salesMgr, ownerTok, nil); r.status != http.StatusNoContent {
				t.Fatalf("revoke: %d %s", r.status, r.body)
			}
		}},
		{"a role is added", sales, func(t *testing.T, m createdMember, _ string) {
			f.assignRole(t, ownerTok, m.membershipID, w.roles[role.SystemRoles.Tenant.Administrator])
		}},
		{"an override is added", sales, func(t *testing.T, m createdMember, _ string) {
			f.grantOverrides(t, ownerTok, m.membershipID, append(atkRoutePerms(), p.Crm.Leads.Assign), nil)
		}},
		{"an override is removed", sales, func(t *testing.T, m createdMember, _ string) {
			f.grantOverrides(t, ownerTok, m.membershipID, []string{}, nil)
		}},
		{"the role loses a permission", customWithInventory, func(t *testing.T, _ createdMember, ownRole string) {
			if r := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+ownRole+"/permissions/revoke", ownerTok, ports.RolePermissionRequest{Permission: p.Inventory.Catalog.Read}); r.status != http.StatusNoContent {
				t.Fatalf("revoke role permission: %d %s", r.status, r.body)
			}
		}},
		{"the role is re-levelled", customWithInventory, func(t *testing.T, _ createdMember, ownRole string) {
			lvl := 45
			if r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+ownRole, ownerTok, ports.UpdateRoleRequest{HierarchyLevel: &lvl}); r.status != http.StatusNoContent {
				t.Fatalf("re-level: %d %s", r.status, r.body)
			}
		}},
		{"the member is deactivated", sales, func(t *testing.T, m createdMember, _ string) {
			if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", ownerTok, ports.DeactivateUserRequest{Reason: "stale attack"}); r.status != http.StatusNoContent {
				t.Fatalf("deactivate: %d %s", r.status, r.body)
			}
		}},
	}
	probes := []struct {
		what, method, path string
		body               any
		okStatus           int
	}{
		{"an authority command", http.MethodPatch, "/api/v1/users/" + jr.membershipID + "/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}, http.StatusNoContent},
		{"an identity read", http.MethodGet, "/api/v1/users?page_size=5", nil, http.StatusOK},
		{"another module's route", http.MethodGet, "/api/v1/inventory/products", nil, http.StatusOK},
	}
	for _, c := range changes {
		for _, warm := range []bool{true, false} {
			state := "cache cold"
			if warm {
				state = "cache warm"
			}
			// arch-test:serial — the cases share one world and one cascade; each needs the previous one's events delivered.
			t.Run(c.name+" / "+state, func(t *testing.T) {
				ownRole := c.roleID(t)
				m, tok := w.spawn(t, "stale", ownRole)
				if warm {
					for _, pr := range probes { // the probes would succeed: the stamp cache now holds the member's stamp
						if r := f.authedJSON(t, pr.method, pr.path, tok, pr.body); r.status != pr.okStatus {
							t.Fatalf("sanity: %s before the change: %d %s, want %d", pr.what, r.status, r.body, pr.okStatus)
						}
					}
				}
				c.apply(t, m, ownRole)
				if warm {
					if s := f.untilStale(t, tok); s.status != http.StatusUnauthorized {
						t.Fatalf("the old token after %s: %d %s, want 401", c.name, s.status, s.body)
					}
				}
				for _, pr := range probes {
					r := f.authedJSON(t, pr.method, pr.path, tok, pr.body)
					if r.status != http.StatusUnauthorized {
						t.Errorf("%s with the old token after %s (%s): %d %s, want 401", pr.what, c.name, state, r.status, r.body)
					}
				}
			})
		}
	}
}
