//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goals 8 and 10: learn what one may not see from the difference
// between "does not exist" and "exists but not yours", and smuggle authority in
// through the spelling of an id or the shape of a body.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

const nilUUID = "00000000-0000-0000-0000-000000000000"

// idSpellings returns the spellings of one UUID an attacker may try, keyed by name.
func idSpellings(id string) map[string]string {
	return map[string]string{
		"canonical":  id,
		"upper":      strings.ToUpper(id),
		"urn":        "urn:uuid:" + id,
		"braces":     "%7B" + id + "%7D",
		"hyphenless": strings.ReplaceAll(id, "-", ""),
		"padded":     "%20" + id + "%20",
		"trailing /": id + "%2F",
		"dot-dot":    "..%2F" + id,
		"double":     id + "," + id,
	}
}

// idRoute is an authority route that takes one id in its path.
type idRoute struct {
	what, method, tmpl string // tmpl has one %s for the id
	body               any
}

func memberIDRoutes(roleID string) []idRoute {
	return []idRoute{
		{"deactivate", http.MethodPost, "/api/v1/users/%s/deactivate", ports.DeactivateUserRequest{Reason: "id game"}},
		{"reactivate", http.MethodPost, "/api/v1/users/%s/reactivate", nil},
		{"assign role", http.MethodPost, "/api/v1/users/%s/roles", ports.AssignUserRoleRequest{RoleID: roleID}},
		{"revoke role", http.MethodDelete, "/api/v1/users/%s/roles/" + roleID, nil},
		{"overrides", http.MethodPatch, "/api/v1/users/%s/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}},
		{"clear manager", http.MethodDelete, "/api/v1/users/%s/manager", nil},
		{"profile", http.MethodPatch, "/api/v1/users/%s/profile", ports.UpdateUserProfileRequest{Designation: "id game"}},
	}
}

func roleIDRoutes() []idRoute {
	p := permission.IdentityPermissions
	return []idRoute{
		{"rename", http.MethodPatch, "/api/v1/roles/%s", map[string]any{"name": "Id Game"}},
		{"re-level", http.MethodPatch, "/api/v1/roles/%s", map[string]any{"hierarchy_level": 70}},
		{"replace permissions", http.MethodPut, "/api/v1/roles/%s/permissions", ports.ReplaceRolePermissionsRequest{Permissions: []string{}}},
		{"grant", http.MethodPost, "/api/v1/roles/%s/permissions/grant", ports.RolePermissionRequest{Permission: p.Tasks.WorkItems.Read}},
		{"revoke", http.MethodPost, "/api/v1/roles/%s/permissions/revoke", ports.RolePermissionRequest{Permission: p.Tasks.WorkItems.Read}},
		{"parent", http.MethodPatch, "/api/v1/roles/%s/parent", map[string]any{"parent_role_id": nil}},
		{"deactivate", http.MethodPost, "/api/v1/roles/%s/deactivate", nil},
	}
}

// TestAttack_Inputs_IdSpellingsNeverBypassARefusal is goal 10 for ids in paths and
// bodies. "sales" (rank 20) is refused on the Senior Manager (15) and on the Senior
// Manager ROLE in their canonical spelling; no other spelling may turn the refusal
// into anything but a 4xx, and nothing may change. Ids that name nothing (nil UUID,
// a random UUID, another tenant's id) are 4xx too; an inactive role is 409 role_inactive.
func TestAttack_Inputs_IdSpellingsNeverBypassARefusal(t *testing.T) {
	w := newAttackWorld(t)
	execRole := w.roles[role.SystemRoles.Tenant.SalesExecutive]
	seniorRole := w.roles[role.SystemRoles.Tenant.SeniorManager]
	senior := w.mid(t, "senior")
	// an inactive role to name: deactivate the holder-less Shell role as the owner
	if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+w.roles[shellRoleName]+"/deactivate", w.owner.AccessToken, nil); r.status != http.StatusOK {
		t.Fatalf("deactivate the shell role: %d %s", r.status, r.body)
	}
	inactiveRole := w.roles[shellRoleName]
	foreignMember, foreignRole := w.vExec.membershipID, w.f.roleIDByName(t, w.victim.AccessToken, role.SystemRoles.Tenant.SalesExecutive)

	var rows []attack
	add := func(what, method, path string, body any, status int) {
		rows = append(rows, attack{who: "sales", what: what, method: method, path: path, body: body, status: status, alt: http.StatusBadRequest})
	}
	for name, spelt := range idSpellings(senior) {
		for _, rt := range memberIDRoutes(execRole) {
			add(rt.what+" / member id "+name, rt.method, fmt.Sprintf(rt.tmpl, spelt), rt.body, http.StatusForbidden)
		}
	}
	for name, spelt := range idSpellings(seniorRole) {
		for _, rt := range roleIDRoutes() {
			add(rt.what+" / role id "+name, rt.method, fmt.Sprintf(rt.tmpl, spelt), rt.body, http.StatusForbidden)
		}
	}
	// ids that name nothing the caller may touch
	for what, id := range map[string]string{"nil uuid": nilUUID, "random uuid": ids.NewV7().String(), "another tenant's member": foreignMember} {
		for _, rt := range memberIDRoutes(execRole) {
			add(rt.what+" / "+what, rt.method, fmt.Sprintf(rt.tmpl, id), rt.body, http.StatusNotFound)
		}
	}
	for what, id := range map[string]string{"nil uuid": nilUUID, "random uuid": ids.NewV7().String(), "another tenant's role": foreignRole} {
		for _, rt := range roleIDRoutes() {
			add(rt.what+" / "+what, rt.method, fmt.Sprintf(rt.tmpl, id), rt.body, http.StatusNotFound)
		}
	}
	// ids in bodies: the role to assign, the manager to appoint
	for name, spelt := range idSpellings(seniorRole) {
		add("assign Senior Manager role / body role_id "+name, http.MethodPost, "/api/v1/users/"+w.mid(t, "norole")+"/roles", ports.AssignUserRoleRequest{RoleID: spelt}, http.StatusForbidden)
	}
	for what, id := range map[string]string{"nil uuid": nilUUID, "random uuid": ids.NewV7().String(), "another tenant's role": foreignRole} {
		add("assign / body role_id "+what, http.MethodPost, "/api/v1/users/"+w.mid(t, "norole")+"/roles", ports.AssignUserRoleRequest{RoleID: id}, http.StatusNotFound)
	}
	for name, spelt := range idSpellings(w.mid(t, "sales")) {
		// "sales" appoints themselves (a spelling of their own id) over a member they do not outrank
		add("set manager of a senior / body manager_id "+name, http.MethodPut, "/api/v1/users/"+senior+"/manager", ports.AssignUserManagerRequest{ManagerID: spelt}, http.StatusForbidden)
	}
	for what, id := range map[string]string{"nil uuid": nilUUID, "random uuid": ids.NewV7().String(), "another tenant's member": foreignMember, "an inactive member": w.gone.membershipID} {
		add("set manager of an exec / body manager_id "+what, http.MethodPut, "/api/v1/users/"+w.mid(t, "exec2")+"/manager", ports.AssignUserManagerRequest{ManagerID: id}, http.StatusUnprocessableEntity)
	}
	// every refusal above is a 4xx and moved nothing; the 4xx class is checked row by row
	w.assertIdRows(t, rows)

	// An INACTIVE role (a role is never deleted, ADR 0073) is a real, readable role that takes no
	// edit and no new holder: 409 role_inactive on every edit route and on assign, and deactivating
	// it again is the idempotent 200. Nothing moves.
	for _, rt := range roleIDRoutes() {
		r := w.f.doWithHeaders(t, rt.method, fmt.Sprintf(rt.tmpl, inactiveRole), w.owner.AccessToken, rt.body, nil)
		if rt.what == "deactivate" {
			if r.status != http.StatusOK {
				t.Errorf("[%s / an inactive role] %d %s, want the idempotent 200", rt.what, r.status, r.body)
			}
			continue
		}
		if r.status != http.StatusConflict || decodeError(t, r.body).Error != "role_inactive" {
			t.Errorf("[%s / an inactive role] %d %s, want 409 role_inactive", rt.what, r.status, r.body)
		}
	}
	if r := w.f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+w.mid(t, "norole")+"/roles", w.owner.AccessToken, ports.AssignUserRoleRequest{RoleID: inactiveRole}, nil); r.status != http.StatusConflict || decodeError(t, r.body).Error != "role_inactive" {
		t.Errorf("[assign / an inactive role] %d %s, want 409 role_inactive", r.status, r.body)
	}
}

// assertIdRows runs rows whose acceptable statuses are a family: status is the
// expected one and alt (400) is also fine, but a 2xx or 5xx never is.
func (w *attackWorld) assertIdRows(t *testing.T, rows []attack) {
	t.Helper()
	before := w.snapshot(t)
	for _, a := range rows {
		r := w.f.doWithHeaders(t, a.method, a.path, w.tok(t, a.who), a.body, nil)
		if r.status < 400 || r.status >= 500 {
			t.Errorf("[%s] %s %s: %d %s, want a 4xx refusal", a.what, a.method, a.path, r.status, r.body)
			continue
		}
		if r.status != a.status && r.status != a.alt && r.status != http.StatusUnprocessableEntity && r.status != http.StatusNotFound && r.status != http.StatusForbidden {
			t.Errorf("[%s] %s %s: %d %s, want %d (or %d)", a.what, a.method, a.path, r.status, r.body, a.status, a.alt)
		}
	}
	if after := w.snapshot(t); after != before {
		t.Errorf("the id games changed the world: %s", snapshotDiff(before, after))
	}
}

// bodyGame is one hand-written body.
type bodyGame struct{ what, body string }

// TestAttack_Inputs_BodyGamesAreRefusedAndMoveNothing is goal 10 for bodies: empty,
// null, arrays, scalars, missing keys, wrong types, duplicate keys and malformed
// JSON against every authority route that takes a body. The actor is an
// Administrator acting on a role-less member and the holder-less Shell role, so a
// body the server wrongly accepts WOULD succeed and be seen.
func TestAttack_Inputs_BodyGamesAreRefusedAndMoveNothing(t *testing.T) {
	w := newAttackWorld(t)
	norole, shell := w.mid(t, "norole"), w.roles[shellRoleName]
	execRole, ownerRole := w.roles[role.SystemRoles.Tenant.SalesExecutive], w.roles[role.SystemRoles.Tenant.CompanyOwner]
	// every overrides game carries the member's real overrides_version, so the game is the only thing wrong with it
	ver := `,"overrides_version":"` + w.f.overridesVersion(t, w.tok(t, "admin"), norole) + `"}`
	generic := []bodyGame{
		{"empty body", ""}, {"object missing every key", `{}`}, {"null", `null`}, {"array", `[]`}, {"string", `"x"`}, {"number", `7`},
		{"truncated", `{"role_id":`}, {"trailing garbage", `{"x":1} {"y":2}`},
	}
	type route struct {
		what, method, path string
		games              []bodyGame
	}
	routes := []route{
		{"assign role", http.MethodPost, "/api/v1/users/" + norole + "/roles", append(slicesClone(generic),
			bodyGame{"null role_id", `{"role_id":null}`}, bodyGame{"numeric role_id", `{"role_id":123}`}, bodyGame{"array role_id", `{"role_id":["` + execRole + `"]}`},
			bodyGame{"empty role_id", `{"role_id":""}`}, bodyGame{"duplicate keys, the Company Owner role wins", `{"role_id":"` + execRole + `","role_id":"` + ownerRole + `"}`})},
		{"overrides", http.MethodPatch, "/api/v1/users/" + norole + "/permission-overrides", append(slicesClone(generic),
			bodyGame{"granted missing", `{"revoked":[]` + ver}, bodyGame{"revoked missing", `{"granted":[]` + ver}, bodyGame{"nulls", `{"granted":null,"revoked":null` + ver},
			bodyGame{"string lists", `{"granted":"tenant.admin","revoked":[]` + ver}, bodyGame{"object entries with an expiry", `{"granted":[{"permission":"tenant.admin","expires_at":"2099-01-01T00:00:00Z"}],"revoked":[]` + ver},
			bodyGame{"numeric entries", `{"granted":[1],"revoked":[]` + ver},
			bodyGame{"version missing", `{"granted":[],"revoked":["tasks.work_items.read"]}`}, bodyGame{"version null", `{"granted":[],"revoked":["tasks.work_items.read"],"overrides_version":null}`},
			bodyGame{"version empty", `{"granted":[],"revoked":["tasks.work_items.read"],"overrides_version":""}`}, bodyGame{"version numeric", `{"granted":[],"revoked":["tasks.work_items.read"],"overrides_version":5}`},
			bodyGame{"version as a list", `{"granted":[],"revoked":["tasks.work_items.read"],"overrides_version":["` + strings.TrimSuffix(strings.TrimPrefix(ver, `,"overrides_version":"`), `"}`) + `"]}`},
			bodyGame{"duplicate version keys, the real one first", `{"granted":[],"revoked":["tasks.work_items.read"]` + strings.TrimSuffix(ver, "}") + `,"overrides_version":"x"}`})},
		{"set manager", http.MethodPut, "/api/v1/users/" + norole + "/manager", append(slicesClone(generic),
			bodyGame{"null manager", `{"manager_id":null}`}, bodyGame{"empty manager", `{"manager_id":""}`}, bodyGame{"numeric manager", `{"manager_id":5}`})},
		{"deactivate", http.MethodPost, "/api/v1/users/" + norole + "/deactivate", append(slicesClone(generic),
			bodyGame{"empty reason", `{"reason":""}`}, bodyGame{"blank reason", `{"reason":"   "}`}, bodyGame{"numeric reason", `{"reason":5}`})},
		{"create role", http.MethodPost, "/api/v1/roles", append(slicesClone(generic),
			bodyGame{"no level", `{"name":"Body Game"}`}, bodyGame{"level 0", `{"name":"Body Game","hierarchy_level":0}`}, bodyGame{"level -1", `{"name":"Body Game","hierarchy_level":-1}`},
			bodyGame{"level 100", `{"name":"Body Game","hierarchy_level":100}`}, bodyGame{"level 1.5", `{"name":"Body Game","hierarchy_level":1.5}`},
			bodyGame{"level as string", `{"name":"Body Game","hierarchy_level":"60"}`}, bodyGame{"no name", `{"hierarchy_level":60}`}, bodyGame{"empty name", `{"name":"","hierarchy_level":60}`},
			bodyGame{"duplicate level, the senior one wins", `{"name":"Body Game","hierarchy_level":60,"hierarchy_level":5}`},
			bodyGame{"parent is not a uuid", `{"name":"Body Game","hierarchy_level":60,"parent_role_id":"x"}`})},
		{"edit role", http.MethodPatch, "/api/v1/roles/" + shell, append(slicesClone(generic),
			bodyGame{"level 0", `{"hierarchy_level":0}`}, bodyGame{"level 100", `{"hierarchy_level":100}`}, bodyGame{"level -1", `{"hierarchy_level":-1}`},
			bodyGame{"level at the actor's own", `{"hierarchy_level":10}`}, bodyGame{"level above the actor", `{"hierarchy_level":5}`},
			bodyGame{"level as string", `{"hierarchy_level":"70"}`}, bodyGame{"name as number", `{"name":5}`})},
		{"replace permissions", http.MethodPut, "/api/v1/roles/" + shell + "/permissions", append(slicesClone(generic),
			bodyGame{"missing key", `{}`}, bodyGame{"null list", `{"permissions":null}`}, bodyGame{"string instead of a list", `{"permissions":"tenant.admin"}`},
			bodyGame{"list with a reserved name", `{"permissions":["tenant.admin"]}`}, bodyGame{"list with an object", `{"permissions":[{"name":"tenant.admin"}]}`})},
		{"grant permission", http.MethodPost, "/api/v1/roles/" + shell + "/permissions/grant", append(slicesClone(generic),
			bodyGame{"null permission", `{"permission":null}`}, bodyGame{"list permission", `{"permission":["tenant.admin"]}`}, bodyGame{"reserved permission", `{"permission":"tenant.admin"}`},
			bodyGame{"duplicate keys, the reserved name wins", `{"permission":"tasks.work_items.read","permission":"tenant.admin"}`})},
		{"set role parent", http.MethodPatch, "/api/v1/roles/" + shell + "/parent", append(slicesClone(generic),
			bodyGame{"key missing", `{"reason":"no parent key at all"}`}, bodyGame{"parent not a uuid", `{"parent_role_id":"x"}`}, bodyGame{"parent numeric", `{"parent_role_id":5}`})},
		{"submit request", http.MethodPost, "/api/v1/permission-requests", append(slicesClone(generic),
			bodyGame{"permission null", `{"permission":null,"reason":"` + elevationReason + `"}`}, bodyGame{"permission list", `{"permission":["tenant.admin"],"reason":"` + elevationReason + `"}`},
			bodyGame{"reason missing", `{"permission":"identity.users.view"}`}, bodyGame{"duration as string", `{"permission":"identity.users.view","reason":"` + elevationReason + `","duration_days":"7"}`})},
	}
	var rows []attack
	for _, rt := range routes {
		for _, g := range rt.games {
			var body any
			if g.body != "" {
				body = rawBody(g.body)
			}
			rows = append(rows, attack{who: "admin", what: rt.what + ": " + g.what, method: rt.method, path: rt.path, body: body})
		}
	}
	before := w.snapshot(t)
	for _, a := range rows {
		r := w.f.doWithHeaders(t, a.method, a.path, w.tok(t, a.who), a.body, nil)
		// Several games are legitimate requests in disguise (a duplicate key whose LAST value is fine, a
		// level at the actor's own, ...): those are refused for another reason or not at all. What must
		// never happen is a 5xx, or a state change not explained by one of the two known-legal rows.
		if r.status >= 500 {
			t.Errorf("[%s] %s %s: %d %s, want a 4xx", a.what, a.method, a.path, r.status, r.body)
		}
		// PATCH /roles/{id} is a partial update, but a body that names nothing the route recognises
		// (an empty object, null, a body followed by garbage) is refused 400 invalid_body (hierarchy
		// round 6), not accepted as a silent no-op.
		noFieldBody := strings.HasPrefix(a.what, "edit role: ") && (strings.HasSuffix(a.what, "object missing every key") || strings.HasSuffix(a.what, ": null") || strings.HasSuffix(a.what, "trailing garbage"))
		if noFieldBody && (r.status != http.StatusBadRequest || errCodeOf(r) != "invalid_body") {
			t.Errorf("[%s] %s %s: %d %s, want 400 invalid_body", a.what, a.method, a.path, r.status, r.body)
		}
		if r.status >= 200 && r.status < 300 {
			t.Errorf("[%s] %s %s: %d %s, a hand-written body was accepted", a.what, a.method, a.path, r.status, r.body)
		}
	}
	if after := w.snapshot(t); after != before {
		t.Errorf("the body games changed the world: %s", snapshotDiff(before, after))
	}
}

func slicesClone(in []bodyGame) []bodyGame { return append([]bodyGame(nil), in...) }

// TestAttack_Inputs_ExtraFieldsConferNothing is goal 10 for fields the DTOs do not
// declare. The requests are legitimate (the actor may do exactly what the declared
// fields ask), so they succeed; what must not happen is the extra field taking
// effect: another tenant, the super-admin flag, the system-default flag, a
// requester or state on a permission request, an expiry on an override.
func TestAttack_Inputs_ExtraFieldsConferNothing(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	f := w.f
	adm := w.tok(t, "admin")
	vBefore := w.snapshotOf(t, w.victim)

	// create role with smuggled flags and a foreign tenant
	r := f.authedJSON(t, http.MethodPost, "/api/v1/roles", adm, rawBody(fmt.Sprintf(
		`{"name":"Smuggler","hierarchy_level":60,"is_super_admin":true,"is_system_default":true,"tenant_id":%q,"permissions":["tenant.admin"],"hierarchy_level_override":0}`, w.victim.TenantID)))
	if r.status != http.StatusCreated {
		t.Fatalf("create role with extra fields: %d %s, want 201 (the extra fields are ignored)", r.status, r.body)
	}
	var created ports.CreateRoleResponse
	if err := json.Unmarshal(r.body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := f.roleDto(t, w.owner.AccessToken, created.RoleID)
	if got.IsSuperAdmin || got.IsSystemDefault || got.TenantID != w.owner.TenantID || len(got.Permissions) != 0 || got.HierarchyLevel != 60 {
		t.Errorf("the smuggled fields took effect: %+v", got)
	}

	// assign role with a foreign tenant and super-user flags in the body
	norole := w.mid(t, "norole")
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+norole+"/roles", adm, rawBody(fmt.Sprintf(
		`{"role_id":%q,"tenant_id":%q,"is_super_user":true,"hierarchy_level":0,"expires_at":"2099-01-01T00:00:00Z"}`, w.roles[role.SystemRoles.Tenant.SalesExecutive], w.victim.TenantID))); r.status != http.StatusNoContent {
		t.Fatalf("assign with extra fields: %d %s, want 204", r.status, r.body)
	}
	if u := f.getUser(t, w.owner.AccessToken, norole); len(u.RoleIDs) != 1 || u.RoleIDs[0] != w.roles[role.SystemRoles.Tenant.SalesExecutive] {
		t.Errorf("the assignment with extra fields produced roles %v", u.RoleIDs)
	}

	// override with an expiry the route does not have: the grant must be permanent, nothing else changes
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+norole+"/permission-overrides", adm, rawBody(fmt.Sprintf(
		`{"granted":[%q],"revoked":[],"overrides_version":%q,"expires_at":"2000-01-01T00:00:00Z","tenant_id":%q}`, p.Tasks.WorkItems.Read, f.overridesVersion(t, adm, norole), w.victim.TenantID))); r.status != http.StatusNoContent {
		t.Fatalf("overrides with extra fields: %d %s, want 204", r.status, r.body)
	}
	if ov := w.overridesOf(t, w.owner.TenantID, norole); !strings.Contains(ov, "granted=["+p.Tasks.WorkItems.Read+"@0001-01-01T00:00:00Z]") {
		t.Errorf("override state = %s, want one permanent grant (zero expiry)", ov)
	}

	// a request with a smuggled requester and state is the caller's own, pending
	exec2 := w.mid(t, "exec2")
	sub := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", w.tok(t, "exec"), rawBody(fmt.Sprintf(
		`{"permission":%q,"reason":%q,"requester_membership_id":%q,"state":"approved","approver_membership_id":%q,"tenant_id":%q}`,
		p.Inventory.Catalog.Manage, elevationReason, exec2, w.mid(t, "exec"), w.victim.TenantID)))
	if sub.status != http.StatusCreated {
		t.Fatalf("submit with extra fields: %d %s, want 201", sub.status, sub.body)
	}
	var cr ports.CreatePermissionRequestResponse
	if err := json.Unmarshal(sub.body, &cr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	g := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+cr.RequestID, w.owner.AccessToken, nil)
	var d ports.PermissionRequestDto
	if err := json.Unmarshal(g.body, &d); err != nil || d.State != "pending" || d.RequesterMembershipID != w.mid(t, "exec") || d.TenantID != w.owner.TenantID {
		t.Errorf("the smuggled request fields took effect: %d %s", g.status, g.body)
	}
	if after := w.snapshotOf(t, w.victim); after != vBefore {
		t.Errorf("the victim tenant changed: %s", snapshotDiff(vBefore, after))
	}
}

// A name listed twice in "granted" must not turn a REMOVAL into a silent no-op:
// the member holds {read, manage}; the save lists {read, read}; manage must go
// (regression: the same-length, every-entry-present comparison answered 204 and
// wrote nothing, so the member kept the permission the caller meant to remove).
func TestAttack_Inputs_DuplicateGrantedNamesStillRemove(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	f := w.f
	adm := w.tok(t, "admin")
	target := w.mid(t, "norole")
	path := "/api/v1/users/" + target + "/permission-overrides"

	if r := f.authedJSON(t, http.MethodPatch, path, adm, ports.ReplaceUserPermissionOverridesRequest{
		Granted: []string{p.Tasks.WorkItems.Read, p.Tasks.WorkItems.Manage}, Revoked: []string{}}); r.status != http.StatusNoContent {
		t.Fatalf("setup grant: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPatch, path, adm, rawBody(fmt.Sprintf(`{"granted":[%q,%q],"revoked":[],"overrides_version":%q}`,
		p.Tasks.WorkItems.Read, p.Tasks.WorkItems.Read, f.overridesVersion(t, adm, target)))); r.status != http.StatusNoContent {
		t.Fatalf("duplicate-name save: %d %s, want 204", r.status, r.body)
	}
	ov := w.overridesOf(t, w.owner.TenantID, target)
	if strings.Contains(ov, p.Tasks.WorkItems.Manage) || !strings.Contains(ov, p.Tasks.WorkItems.Read) {
		t.Errorf("override state = %s, want only %s (the removed grant must be gone)", ov, p.Tasks.WorkItems.Read)
	}
}
