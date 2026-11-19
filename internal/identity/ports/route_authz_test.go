package ports_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

type tokenVerifier map[string]*jwt.Claims

func (v tokenVerifier) Verify(tok string) (*jwt.Claims, error) {
	if c, ok := v[tok]; ok {
		return c, nil
	}
	return nil, errors.New("unknown token")
}

const (
	azTenant = "0190a000-0000-7000-8000-000000000001"
	azSelf   = "0190a000-0000-7000-8000-0000000000aa"
	azOther  = "0190a000-0000-7000-8000-0000000000bb"
	azRole   = "0190a000-0000-7000-8000-0000000000cc"
)

func azClaims(perms ...string) *jwt.Claims {
	c := &jwt.Claims{TenantID: azTenant, TenantSlug: "acme", MembershipID: azSelf, SecurityStamp: "stamp", Permissions: perms}
	c.Subject = "0190a000-0000-7000-8000-0000000000ff"
	return c
}

func azServe(t *testing.T, c *jwt.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return azServeApp(t, app.Application{}, c, method, path, body)
}

func azServeApp(t *testing.T, a app.Application, c *jwt.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	ports.AddRoutes(mux, logger, a, tokenVerifier{"tok": c}, stubStampValidator{})
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	httpmw.Recover(logger)(mux).ServeHTTP(rec, req)
	return rec
}

func gatePassed(code int) bool {
	return code != http.StatusUnauthorized && code != http.StatusForbidden
}

func TestRouteAuthz_IdentityRoutesRequirePermissions(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	u, r, tn := "/api/v1/users/", "/api/v1/roles/", "/api/v1/tenants/"+azTenant
	routes := []struct{ method, path, perm string }{
		{"GET", "/api/v1/users", p.Users.View},
		{"GET", u + azOther, p.Users.View},
		{"PATCH", u + azOther + "/profile", p.Users.Update},
		{"POST", u + azOther + "/deactivate", p.Users.Deactivate},
		{"POST", u + azOther + "/reactivate", p.Users.Reactivate},
		{"POST", u + azOther + "/roles", p.Roles.Assign},
		{"DELETE", u + azOther + "/roles/" + azRole, p.Roles.Revoke},
		{"PATCH", u + azOther + "/permission-overrides", p.Users.UpdatePermissions},
		{"PUT", u + azOther + "/manager", p.Users.Update},
		{"DELETE", u + azOther + "/manager", p.Users.Update},
		{"POST", "/api/v1/users", p.Users.Create},
		{"GET", "/api/v1/roles", p.Roles.View},
		{"GET", r + azRole, p.Roles.View},
		{"POST", "/api/v1/roles", p.Roles.Create},
		{"PATCH", r + azRole, p.Roles.Update},
		{"PUT", r + azRole + "/permissions", p.Roles.Update},
		{"POST", r + azRole + "/permissions/grant", p.Roles.Update},
		{"POST", r + azRole + "/permissions/revoke", p.Roles.Update},
		{"PATCH", r + azRole + "/parent", p.Roles.Update},
		{"DELETE", r + azRole, p.Roles.Delete},
		{"GET", tn, p.Tenants.View},
		{"PATCH", tn + "/profile", p.Tenants.Update},
		{"PATCH", tn + "/statutory", p.Tenants.Update},
		{"PATCH", tn + "/admin-contact", p.Tenants.Update},
		{"PATCH", tn + "/settings", p.Tenants.UpdateSettings},
		{"PATCH", tn + "/display-preferences", p.Tenants.Update},
		{"GET", tn + "/audit/events", p.Tenants.View},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			t.Parallel()
			if rec := azServe(t, azClaims(), rt.method, rt.path, "{}"); rec.Code != http.StatusForbidden {
				t.Errorf("member without %s: status %d, want 403", rt.perm, rec.Code)
			}
			if rec := azServe(t, azClaims(rt.perm), rt.method, rt.path, "{}"); !gatePassed(rec.Code) {
				t.Errorf("holder of %s: status %d, want gate pass (body %s)", rt.perm, rec.Code, rec.Body.String())
			}
			if rec := azServe(t, azClaims(p.Meta.TenantAdmin), rt.method, rt.path, "{}"); !gatePassed(rec.Code) {
				t.Errorf("tenant.admin: status %d, want gate pass (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRouteAuthz_SelfServiceAndSelfTarget(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	u := "/api/v1/users/" + azSelf
	for _, rt := range []struct{ method, path string }{{"GET", u}, {"PATCH", u + "/profile"}} {
		if rec := azServe(t, azClaims(), rt.method, rt.path, "{}"); !gatePassed(rec.Code) {
			t.Errorf("self %s %s: status %d, want gate pass", rt.method, rt.path, rec.Code)
		}
	}
	// Self-target is refused by the authority policy inside each command
	// (ADR 0069), so the routes run over real handlers on fakes.
	a := selfTargetApp(t)
	manager := `{"manager_id":"` + azOther + `"}`
	for _, rt := range []struct{ method, path, body string }{
		{"POST", u + "/deactivate", `{"reason":"self lockout attempt"}`}, {"POST", u + "/reactivate", ""},
		{"POST", u + "/roles", `{"role_id":"` + azRole + `"}`},
		{"DELETE", u + "/roles/" + azRole, ""}, {"PATCH", u + "/permission-overrides", `{"granted":[],"revoked":[]}`},
		{"PUT", u + "/manager", manager}, {"DELETE", u + "/manager", ""},
	} {
		rec := azServeApp(t, a, bodyAdminClaims(p.Meta.TenantAdmin), rt.method, rt.path, rt.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "self_modification_forbidden") {
			t.Errorf("self-target %s %s: status %d body %s, want 403 self_modification_forbidden", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

// selfTargetApp wires the seven authority commands over fakes holding the
// caller's own membership (azSelf), a peer (azOther) and a role (azRole).
func selfTargetApp(t *testing.T) app.Application {
	t.Helper()
	mems := membershiptest.NewFakeRepository()
	for _, id := range []string{azSelf, azOther} {
		m, err := membership.New(membership.ID(id), person.ID(id), tenant.ID(azTenant), "", bodyNow)
		if err != nil {
			t.Fatalf("membership.New: %v", err)
		}
		if err := mems.Add(t.Context(), m); err != nil {
			t.Fatalf("mems.Add: %v", err)
		}
	}
	roles := roletest.NewFakeRepository()
	r, err := role.New(role.ID(azRole), tenant.ID(azTenant), "Rep", false, 60, false, bodyNow)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatalf("roles.Add: %v", err)
	}
	now := func() time.Time { return bodyNow }
	var a app.Application
	a.Commands.DeactivateUser = command.NewDeactivateUserHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.ReactivateUser = command.NewReactivateUserHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.AssignUserRole = command.NewAssignUserRoleHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.RevokeUserRole = command.NewRevokeUserRoleHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.ReplaceUserPermissionOverrides = command.NewReplaceUserPermissionOverridesHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.AssignUserManager = command.NewAssignUserManagerHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.RemoveUserManager = command.NewRemoveUserManagerHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.ReplaceRolePermissions = command.NewReplaceRolePermissionsHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.GrantRolePermission = command.NewGrantRolePermissionHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	a.Commands.RevokeRolePermission = command.NewRevokeRolePermissionHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, now)
	return a
}

func TestRouteAuthz_GrantBeyondAuthorityRejected(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	// Override escalation is decided by the authority policy inside the command
	// (authority_commands_test, membership_rank_e2e_test); role edits are decided
	// the same way (role_management_test, role_authority_e2e_test) and reach it
	// here through the real handlers: a caller holding only roles.update confers
	// nothing it does not hold.
	a := selfTargetApp(t)
	claims := bodyAdminClaims(p.Roles.Update)
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/api/v1/roles/" + azRole + "/permissions/grant", `{"permission":"identity.users.view"}`},
		{"PUT", "/api/v1/roles/" + azRole + "/permissions", `{"permissions":["identity.users.view"]}`},
	} {
		rec := azServeApp(t, a, claims, rt.method, rt.path, rt.body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "grant_exceeds_authority") {
			t.Errorf("role escalation %s %s: %d %s, want 403 grant_exceeds_authority", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

// capabilitiesApp wires the real cached capabilities query over empty fakes:
// enrichment returns membership-not-found, so the handler takes the documented
// degraded (JWT-only) path and still answers 200.
func capabilitiesApp(t *testing.T) app.Application {
	t.Helper()
	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	hc, err := cache.New(cache.Config{L1MaxItems: 100, L2: cli, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)
	mems := membershiptest.NewFakeRepository()
	inner := query.NewGetCapabilitiesHandler(persontest.NewFakeRepository(), mems, roletest.NewFakeRepository())
	var a app.Application
	a.Queries.GetCapabilities = query.NewCachedGetCapabilitiesHandler(inner, hc, mems)
	return a
}

func TestCapabilities_ExposesEffectivePermissions(t *testing.T) {
	t.Parallel()
	a := capabilitiesApp(t)
	rec := azServeApp(t, a, azClaims(permission.IdentityPermissions.Meta.TenantAdmin), "GET", "/api/v1/auth/me/capabilities", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities: %d %s", rec.Code, rec.Body.String())
	}
	var dto struct {
		EffectivePermissions []string `json:"effective_permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Contains(dto.EffectivePermissions, permission.IdentityPermissions.Users.View) {
		t.Errorf("effective_permissions missing identity.users.view: %v", dto.EffectivePermissions)
	}
}

// Impersonation mints a tenant.admin token for any tenant, so its three routes
// need their own permission: platform.tenants.manage (who may edit tenants)
// opens none of them, and tenant.admin never does.
func TestRouteAuthz_ImpersonationNeedsItsOwnPermission(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	operator := func(perms ...string) *jwt.Claims {
		c := azClaims(perms...)
		c.TenantSlug, c.IsPlatform = authn.PlatformTenantSlug, true
		return c
	}
	body := `{"target_tenant_id":"` + azOther + `","reason":"support ticket 4711"}`
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/api/v1/platform/impersonation/sessions", body},
		{"GET", "/api/v1/platform/impersonation/sessions", ""},
	} {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			t.Parallel()
			for name, c := range map[string]*jwt.Claims{
				"no permission":               operator(),
				"platform.tenants.manage":     operator(p.Platform.TenantsManage),
				"platform.users.manage":       operator(p.Platform.UsersManage, p.Platform.RolesManage),
				"tenant.admin of the tenant":  azClaims(p.Meta.TenantAdmin),
				"platform.impersonation only": azClaims(p.Platform.ImpersonationManage), // not a platform-tenant operator
			} {
				if rec := azServe(t, c, rt.method, rt.path, rt.body); rec.Code != http.StatusForbidden {
					t.Errorf("%s: status %d, want 403 (body %s)", name, rec.Code, rec.Body.String())
				}
			}
			if rec := azServe(t, operator(p.Platform.ImpersonationManage), rt.method, rt.path, rt.body); !gatePassed(rec.Code) {
				t.Errorf("platform operator holding platform.impersonation.manage: status %d, want gate pass (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// An operator must not globally suspend, anonymise or edit their OWN person:
// DenySelfPerson sits inside the platform gate on the four mutating person
// routes. Reads and other people stay open; a super user is exempt.
func TestRouteAuthz_PlatformPersonMutations_DenySelf(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	const otherPerson = "0190a000-0000-7000-8000-0000000000ee"
	selfPerson := azClaims().Subject
	operator := func(super bool, perms ...string) *jwt.Claims {
		c := azClaims(perms...)
		c.TenantSlug, c.IsPlatform, c.IsSuperUser = authn.PlatformTenantSlug, true, super
		return c
	}
	for _, rt := range []struct{ method, suffix, perm string }{
		{"PATCH", "/profile", p.Platform.UsersManage},
		{"POST", "/global-suspend", p.Platform.UsersManage},
		{"POST", "/lift-global-suspension", p.Platform.UsersManage},
		{"POST", "/anonymise", p.Users.Anonymise},
	} {
		t.Run(rt.method+" "+rt.suffix, func(t *testing.T) {
			t.Parallel()
			base := "/api/v1/platform/persons/"
			rec := azServe(t, operator(false, rt.perm), rt.method, base+selfPerson+rt.suffix, "{}")
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "self_modification_forbidden") {
				t.Errorf("own person: %d %s, want 403 self_modification_forbidden", rec.Code, rec.Body.String())
			}
			rec = azServe(t, operator(false, rt.perm), rt.method, base+strings.ToUpper(selfPerson)+rt.suffix, "{}")
			if rec.Code != http.StatusForbidden {
				t.Errorf("own person, upper-case spelling: %d, want 403", rec.Code)
			}
			if rec := azServe(t, operator(false, rt.perm), rt.method, base+otherPerson+rt.suffix, "{}"); !gatePassed(rec.Code) {
				t.Errorf("another person: %d %s, want gate pass", rec.Code, rec.Body.String())
			}
			if rec := azServe(t, operator(true), rt.method, base+selfPerson+rt.suffix, "{}"); !gatePassed(rec.Code) {
				t.Errorf("super user on own person: %d %s, want gate pass", rec.Code, rec.Body.String())
			}
		})
	}
}

// capabilities rank/tier come from the rank claim via the same Actor the
// policy uses; an absent claim (old token) reads as rank 99 / tier user.
func TestCapabilities_RankAndTier(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		rank     *int
		wantRank int
		wantTier string
	}{
		{"owner", capRank(0), 0, "owner"},
		{"admin", capRank(10), 10, "admin"},
		{"senior manager", capRank(15), 15, "manager"},
		{"manager", capRank(20), 20, "manager"},
		{"team lead", capRank(30), 30, "team_lead"},
		{"user", capRank(50), 50, "user"},
		{"none", capRank(99), 99, "user"},
		{"absent claim", nil, 99, "user"},
		{"out of range clamps to none", capRank(-3), 99, "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := azClaims(permission.IdentityPermissions.Meta.TenantAdmin)
			c.Rank = tc.rank
			rec := azServeApp(t, capabilitiesApp(t), c, "GET", "/api/v1/auth/me/capabilities", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("capabilities: %d %s", rec.Code, rec.Body.String())
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode: %v", err)
			}
			var dto struct {
				Rank *int   `json:"rank"`
				Tier string `json:"tier"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, ok := raw["rank"]; !ok || dto.Rank == nil || *dto.Rank != tc.wantRank || dto.Tier != tc.wantTier {
				t.Errorf("rank/tier = %v/%q, want %d/%q (body %s)", dto.Rank, dto.Tier, tc.wantRank, tc.wantTier, rec.Body.String())
			}
		})
	}
}

func capRank(n int) *int { return &n }
