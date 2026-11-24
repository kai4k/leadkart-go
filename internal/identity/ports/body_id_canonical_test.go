package ports_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy/rolehierarchytest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// Ids supplied in a request BODY must reach the command in canonical form,
// whatever spelling the client used (uuid.Parse accepts all of these).

const (
	bodyManager = "0190a000-0000-7000-8000-0000000000dd"
	bodyRoleB   = "0190a000-0000-7000-8000-0000000000ab"
)

var bodyNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func idSpellings(canonical string) map[string]string {
	return map[string]string{
		"canonical":  canonical,
		"uppercase":  strings.ToUpper(canonical),
		"urn":        "urn:uuid:" + canonical,
		"braces":     "{" + canonical + "}",
		"hyphenless": strings.ReplaceAll(canonical, "-", ""),
	}
}

type inlineUoW struct{}

func (inlineUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

type bodyLock struct{}

func (bodyLock) LockTenantAuthority(context.Context, tenant.ID) error { return nil }

// bodyPersons answers GetByID with an active person for any id.
type bodyPersons struct{ person.Repository }

func (bodyPersons) GetByID(_ context.Context, id person.ID) (*person.Person, error) {
	addr, err := email.New(strings.ReplaceAll(string(id), "-", "") + "@body.test")
	if err != nil {
		return nil, err
	}
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=4$YWJjZGVmZ2g$ZS1zdHViLWhhc2gtdGVzdC1maXh0dXJlLW9ubHk")
	if err != nil {
		return nil, err
	}
	return person.New(id, addr, "A", "B", hash, bodyNow)
}

// bodyAdminClaims are claims of an Administrator-rank caller (the actor the
// port builds from them is rank 10).
func bodyAdminClaims(perms ...string) *jwt.Claims {
	c := azClaims(perms...)
	rank := authority.RankAdmin.Int()
	c.Rank = &rank
	return c
}

// bodyImpersonating is bodyAdminClaims as an impersonation session: a LeadKart platform operator acting
// inside the company, the only tenant-side caller that may create, edit or delete roles (owner ruling
// 2026-11-25, ADR 0073 addendum).
func bodyImpersonating(perms ...string) *jwt.Claims {
	c := bodyAdminClaims(perms...)
	c.Act = &jwt.ActClaim{Sub: c.Subject, SessionID: "session", Reason: "support"}
	return c
}

// impersonatingFor returns c as an impersonation session when perm is a platform-managed role
// permission, and c unchanged otherwise.
func impersonatingFor(perm string, c *jwt.Claims) *jwt.Claims {
	if permission.IsPlatformManagedRole(perm) {
		c.Act = &jwt.ActClaim{Sub: c.Subject, SessionID: "session", Reason: "support"}
	}
	return c
}

// bodyRoles is a role repository holding one live custom role per id at the
// given hierarchy level (tenant azTenant).
func bodyRoles(t *testing.T, levels map[string]int) *roletest.FakeRepository {
	t.Helper()
	repo := roletest.NewFakeRepository()
	n := 0
	for id, level := range levels {
		n++
		r, err := role.New(role.ID(id), tenant.ID(azTenant), "Custom Role "+strconv.Itoa(n), false, level, false, bodyNow)
		if err != nil {
			t.Fatalf("role.New: %v", err)
		}
		if err := repo.Add(t.Context(), r); err != nil {
			t.Fatalf("roles.Add: %v", err)
		}
	}
	return repo
}

const bodyMgrRole = "0190a000-0000-7000-8000-0000000000b1"

// bodyManagerWorld holds a manager-level member (bodyManager) so the
// manager-link command has an eligible, more senior manager to load.
func bodyManagerWorld(t *testing.T, repo *membershiptest.FakeRepository) *roletest.FakeRepository {
	t.Helper()
	roles := roletest.NewFakeRepository()
	r, err := role.New(role.ID(bodyMgrRole), tenant.ID(azTenant), "Mgr", false, 20, false, bodyNow)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatalf("roles.Add: %v", err)
	}
	m, err := membership.New(membership.ID(bodyManager), "0190a000-0000-7000-8000-0000000000e2", tenant.ID(azTenant), "", bodyNow)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	if err := m.AssignRole(role.ID(bodyMgrRole), bodyNow); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := repo.Add(t.Context(), m); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return roles
}

func newBodyMembership(t *testing.T, repo *membershiptest.FakeRepository, id string) {
	t.Helper()
	m, err := membership.New(membership.ID(id), "0190a000-0000-7000-8000-0000000000e1", tenant.ID(azTenant), "", bodyNow)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	if err := repo.Add(t.Context(), m); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
}

func TestBodyID_ManagerIDReachesCommandCanonical(t *testing.T) {
	t.Parallel()
	for name, spelled := range idSpellings(bodyManager) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := membershiptest.NewFakeRepository()
			newBodyMembership(t, repo, azOther)
			roles := bodyManagerWorld(t, repo)
			seedBodyActor(t, repo, roles)
			var a app.Application
			a.Commands.AssignUserManager = command.NewAssignUserManagerHandler(inlineUoW{}, bodyLock{}, repo, roles, bodyPersons{}, func() time.Time { return bodyNow })
			rec := azServeApp(t, a, bodyAdminClaims(permission.IdentityPermissions.Users.Update), "PUT",
				"/api/v1/users/"+azOther+"/manager", `{"manager_id":"`+spelled+`"}`)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
			m, err := repo.GetByID(t.Context(), tenant.ID(azTenant), membership.ID(azOther))
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if got := string(m.ReportsTo()); got != bodyManager {
				t.Errorf("ReportsTo = %q, want canonical %q", got, bodyManager)
			}
		})
	}
}

func TestBodyID_ManagerIDSelfSpellingRejected(t *testing.T) {
	t.Parallel()
	for name, spelled := range idSpellings(azOther) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := membershiptest.NewFakeRepository()
			newBodyMembership(t, repo, azOther)
			var a app.Application
			a.Commands.AssignUserManager = command.NewAssignUserManagerHandler(inlineUoW{}, bodyLock{}, repo, roletest.NewFakeRepository(), bodyPersons{}, func() time.Time { return bodyNow })
			rec := azServeApp(t, a, bodyAdminClaims(permission.IdentityPermissions.Users.Update), "PUT",
				"/api/v1/users/"+azOther+"/manager", `{"manager_id":"`+spelled+`"}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d %s, want 422", rec.Code, rec.Body.String())
			}
			m, _ := repo.GetByID(t.Context(), tenant.ID(azTenant), membership.ID(azOther))
			if !m.ReportsTo().IsZero() {
				t.Errorf("ReportsTo = %q, want unset", m.ReportsTo())
			}
		})
	}
}

func TestBodyID_UnparseableBodyIDsAre400(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	for _, tc := range []struct {
		name, method, path, body, code string
		perm                           string
	}{
		{"manager", "PUT", "/api/v1/users/" + azOther + "/manager", `{"manager_id":"nope"}`, "invalid_manager_id", p.Users.Update},
		{"role", "POST", "/api/v1/users/" + azOther + "/roles", `{"role_id":"nope"}`, "invalid_role_id", p.Roles.Assign},
		{"parent", "PATCH", "/api/v1/roles/" + azRole + "/parent", `{"parent_role_id":"nope","reason":"valid reason text"}`, "invalid_role_id", p.Roles.Update},
	} {
		rec := azServe(t, impersonatingFor(tc.perm, azClaims(tc.perm)), tc.method, tc.path, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.code) {
			t.Errorf("%s: %d %s, want 400 %s", tc.name, rec.Code, rec.Body.String(), tc.code)
		}
	}
}

func TestBodyID_RoleIDReachesCommandCanonicalAndDedups(t *testing.T) {
	t.Parallel()
	for name, spelled := range idSpellings(bodyRoleB) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mems := membershiptest.NewFakeRepository()
			newBodyMembership(t, mems, azOther)
			roles := roletest.NewFakeRepository()
			r, err := role.New(role.ID(bodyRoleB), tenant.ID(azTenant), "Rep", false, 50, false, bodyNow)
			if err != nil {
				t.Fatalf("role.New: %v", err)
			}
			if err := roles.Add(t.Context(), r); err != nil {
				t.Fatalf("roles.Add: %v", err)
			}
			seedBodyActor(t, mems, roles)
			var a app.Application
			a.Commands.AssignUserRole = command.NewAssignUserRoleHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, func() time.Time { return bodyNow })
			claims := bodyAdminClaims(permission.IdentityPermissions.Roles.Assign)
			path := "/api/v1/users/" + azOther + "/roles"
			// Canonical assignment first, then the spelled one: the second must
			// dedup (one assignment, canonical) exactly like a canonical repeat.
			for _, body := range []string{`{"role_id":"` + bodyRoleB + `"}`, `{"role_id":"` + spelled + `"}`} {
				if rec := azServeApp(t, a, claims, "POST", path, body); rec.Code != http.StatusNoContent {
					t.Fatalf("assign %s: %d %s", body, rec.Code, rec.Body.String())
				}
			}
			m, _ := mems.GetByID(t.Context(), tenant.ID(azTenant), membership.ID(azOther))
			got := m.RoleAssignments()
			if len(got) != 1 || string(got[0]) != bodyRoleB {
				t.Errorf("RoleAssignments = %v, want exactly [%s]", got, bodyRoleB)
			}
		})
	}
}

func TestBodyID_ParentRoleIDReachesCommandCanonical(t *testing.T) {
	t.Parallel()
	for name, spelled := range idSpellings(bodyRoleB) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			edges := rolehierarchytest.NewFakeRepository()
			roles := bodyRoles(t, map[string]int{azRole: 60, bodyRoleB: 40})
			mems := membershiptest.NewFakeRepository()
			seedBodyActor(t, mems, roles)
			var a app.Application
			a.Commands.SetRoleParent = command.NewSetRoleParentHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, edges,
				func() time.Time { return bodyNow }, func() rolehierarchy.ID { return "0190a000-0000-7000-8000-0000000000f1" })
			rec := azServeApp(t, a, bodyImpersonating(permission.IdentityPermissions.Roles.Update), "PATCH",
				"/api/v1/roles/"+azRole+"/parent", `{"parent_role_id":"`+spelled+`","reason":"reorganise the tree"}`)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
			e, err := edges.GetActiveByChild(t.Context(), tenant.ID(azTenant), role.ID(azRole))
			if err != nil {
				t.Fatalf("GetActiveByChild: %v", err)
			}
			if got := string(e.ParentRoleID()); got != bodyRoleB {
				t.Errorf("ParentRoleID = %q, want canonical %q", got, bodyRoleB)
			}
		})
	}
}

func TestBodyID_CreateRoleParentReachesCommandCanonical(t *testing.T) {
	t.Parallel()
	const newRole = "0190a000-0000-7000-8000-0000000000f2"
	for name, spelled := range idSpellings(bodyRoleB) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			roles, edges := bodyRoles(t, map[string]int{bodyRoleB: 40}), rolehierarchytest.NewFakeRepository()
			mems := membershiptest.NewFakeRepository()
			seedBodyActor(t, mems, roles)
			var a app.Application
			a.Commands.CreateRole = command.NewCreateRoleHandler(inlineUoW{}, bodyLock{}, mems, roles, bodyPersons{}, edges,
				func() time.Time { return bodyNow }, func() role.ID { return newRole }, func() rolehierarchy.ID { return "0190a000-0000-7000-8000-0000000000f1" })
			rec := azServeApp(t, a, bodyImpersonating(permission.IdentityPermissions.Roles.Create), "POST", "/api/v1/roles",
				`{"name":"Field Rep","hierarchy_level":50,"parent_role_id":"`+spelled+`"}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d %s", rec.Code, rec.Body.String())
			}
			e, err := edges.GetActiveByChild(t.Context(), tenant.ID(azTenant), role.ID(newRole))
			if err != nil {
				t.Fatalf("GetActiveByChild: %v", err)
			}
			if got := string(e.ParentRoleID()); got != bodyRoleB {
				t.Errorf("ParentRoleID = %q, want canonical %q", got, bodyRoleB)
			}
		})
	}
}

func TestBodyID_PaddedAndNilBodyIDsAreRejected(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	op := azClaims(p.Platform.ImpersonationManage)
	op.TenantSlug, op.IsPlatform = authn.PlatformTenantSlug, true
	for name, bad := range map[string]string{
		"leading space":  " " + bodyRoleB,
		"trailing space": bodyRoleB + " ",
		"wrapped":        " " + bodyRoleB + " ",
		"nil uuid":       "00000000-0000-0000-0000-000000000000",
	} {
		rec := azServe(t, bodyImpersonating(p.Roles.Create), "POST", "/api/v1/roles", `{"name":"x","hierarchy_level":50,"parent_role_id":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_role_id") {
			t.Errorf("create role, parent %s: %d %s, want 400 invalid_role_id", name, rec.Code, rec.Body.String())
		}
		rec = azServe(t, op, "POST", "/api/v1/platform/impersonation/sessions", `{"target_tenant_id":"`+bad+`","reason":"support ticket 123"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_tenant_id") {
			t.Errorf("impersonation, target %s: %d %s, want 400 invalid_tenant_id", name, rec.Code, rec.Body.String())
		}
	}
}

const bodyActorRole = "0190a000-0000-7000-8000-0000000000a9"

// seedBodyActor stores the caller's own membership (azSelf) holding an
// Administrator-level role with tenant.admin, the facts the commands re-read
// for the acting member inside the locked transaction; bodyAdminClaims is what
// a token for that member carries.
func seedBodyActor(t *testing.T, mems *membershiptest.FakeRepository, roles *roletest.FakeRepository) {
	t.Helper()
	r, err := role.New(role.ID(bodyActorRole), tenant.ID(azTenant), "Body Admin", false, role.HierarchyLevelAdmin, false, bodyNow)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := r.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Meta.TenantAdmin), bodyNow); err != nil {
		t.Fatalf("GrantPermission: %v", err)
	}
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatalf("roles.Add: %v", err)
	}
	m, err := membership.New(membership.ID(azSelf), person.ID(azSelf), tenant.ID(azTenant), "", bodyNow)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	if err := m.AssignRole(r.ID(), bodyNow); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatalf("mems.Add: %v", err)
	}
}

// UpdateByID runs fn on a synthesised person; nothing is persisted (the stamp
// rotation that follows an authority change needs a writable person).
func (p bodyPersons) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	got, err := p.GetByID(ctx, id)
	if err != nil {
		return err
	}
	_, err = fn(got)
	return err
}
