//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// A name a person types that other people read (company, person, role, title,
// reason) cannot carry control or direction-override characters: the route answers
// its own 422 code with errors{<field>: ["invalid_characters"]} and writes nothing.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// rlo is U+202E (right-to-left override), which reorders the text around it.
var rlo = string(rune(0x202e))

// requireInvalidCharacters asserts the 422 shape: the route's code and the one
// field error "invalid_characters".
func requireInvalidCharacters(t *testing.T, what string, r httpResp, wantCode, field string) {
	t.Helper()
	var er ports.ErrorResponse
	if err := json.Unmarshal(r.body, &er); err != nil {
		t.Fatalf("%s: decode %s: %v", what, r.body, err)
	}
	if r.status != http.StatusUnprocessableEntity || er.Error != wantCode ||
		len(er.Errors[field]) != 1 || er.Errors[field][0] != "invalid_characters" {
		t.Errorf("%s: %d %s; want 422 %s with errors{%s:[invalid_characters]}", what, r.status, r.body, wantCode, field)
	}
}

func TestE2E_HumanText_TenantNamesRefused_NothingWritten(t *testing.T) {
	f := newE2EFixture(t)
	tn := f.registerAndLogin(t, "htenant")
	path := "/api/v1/tenants/" + tn.TenantID
	before := f.authedJSON(t, http.MethodGet, path, tn.AccessToken, nil)
	if before.status != http.StatusOK {
		t.Fatalf("get tenant: %d %s", before.status, before.body)
	}

	r := f.authedJSON(t, http.MethodPatch, path+"/profile", tn.AccessToken,
		ports.UpdateTenantProfileRequest{LegalName: "Hacked Pvt Ltd", DisplayName: "Acme" + rlo + "Ltd"})
	requireInvalidCharacters(t, "display_name with U+202E", r, "tenant_invalid", "display_name")
	r = f.authedJSON(t, http.MethodPatch, path+"/profile", tn.AccessToken,
		ports.UpdateTenantProfileRequest{LegalName: "Acme\nPvt Ltd", DisplayName: "Fine"})
	requireInvalidCharacters(t, "legal_name with LF", r, "tenant_invalid", "legal_name")

	after := f.authedJSON(t, http.MethodGet, path, tn.AccessToken, nil)
	if string(before.body) != string(after.body) {
		t.Errorf("a refused write changed the tenant:\nbefore %s\nafter  %s", before.body, after.body)
	}
}

func TestE2E_HumanText_PersonNamesAndTitlesRefused_NothingWritten(t *testing.T) {
	f := newE2EFixture(t)
	tn := f.registerAndLogin(t, "hperson")
	p := permission.IdentityPermissions
	opTok := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.UsersView, p.Platform.UsersManage}, false)

	usersBefore := f.authedJSON(t, http.MethodGet, "/api/v1/users", tn.AccessToken, nil)

	// POST /users: a first name with U+202E.
	email := "bad-name@e2e.test"
	r := f.authedJSON(t, http.MethodPost, "/api/v1/users", tn.AccessToken,
		ports.CreateUserRequest{Email: email, Password: "Member-Pass-2026!x", FirstName: "Amit" + rlo + "Rao", LastName: "Rao"})
	requireInvalidCharacters(t, "create user first_name", r, "user_invalid", "first_name")
	r = f.authedJSON(t, http.MethodPost, "/api/v1/users", tn.AccessToken,
		ports.CreateUserRequest{Email: email, Password: "Member-Pass-2026!x", FirstName: "Amit", LastName: "Ra\x00o"})
	requireInvalidCharacters(t, "create user last_name", r, "user_invalid", "last_name")
	if got := f.authedJSON(t, http.MethodGet, "/api/v1/platform/persons?email="+email, opTok, nil); got.status != http.StatusNotFound {
		t.Errorf("a refused create user left a person behind: %d %s", got.status, got.body)
	}

	// PATCH /users/{id}/profile: designation, department, status_message.
	m := f.createMember(t, tn, "hmember")
	profile := func(d, dep, sm string) httpResp {
		return f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+m.membershipID+"/profile", tn.AccessToken,
			ports.UpdateUserProfileRequest{Designation: d, Department: dep, StatusMessage: sm})
	}
	requireInvalidCharacters(t, "designation", profile("Head"+rlo+"Sales", "Sales", "ok"), "user_invalid", "designation")
	requireInvalidCharacters(t, "department", profile("Head", "Sa\tles", "ok"), "user_invalid", "department")
	requireInvalidCharacters(t, "status_message", profile("Head", "Sales", "in a\nmeeting"), "user_invalid", "status_message")

	// PATCH /platform/persons/{id}/profile (operator).
	r = f.authedJSON(t, http.MethodPatch, "/api/v1/platform/persons/"+m.personID+"/profile", opTok,
		ports.UpdatePersonProfileRequest{FirstName: "Zed" + rlo, LastName: "Member"})
	requireInvalidCharacters(t, "platform person first_name", r, "person_invalid", "first_name")

	// Nothing was written: the member list is as it was.
	usersAfter := f.authedJSON(t, http.MethodGet, "/api/v1/users", tn.AccessToken, nil)
	if usersBefore.status != http.StatusOK || usersAfter.status != http.StatusOK {
		t.Fatalf("list users: %d / %d", usersBefore.status, usersAfter.status)
	}
	got := f.authedJSON(t, http.MethodGet, "/api/v1/platform/persons/"+m.personID, opTok, nil)
	var person ports.PersonDto
	if err := json.Unmarshal(got.body, &person); err != nil || person.FirstName != "hmember" || person.LastName != "Member" {
		t.Errorf("the person changed after refused writes: %s (%v)", got.body, err)
	}
}

func TestE2E_HumanText_RoleNamesRefused_NothingWritten(t *testing.T) {
	f := newE2EFixture(t)
	tn := f.registerAndLogin(t, "hrole")
	rolesBefore := f.authedJSON(t, http.MethodGet, "/api/v1/roles", tn.AccessToken, nil)

	r := f.authedJSON(t, http.MethodPost, "/api/v1/roles", tn.AccessToken,
		ports.CreateRoleRequest{Name: "Sales" + rlo + "Lead", HierarchyLevel: 60})
	requireInvalidCharacters(t, "create role name", r, "role_invalid", "name")

	ok := f.authedJSON(t, http.MethodPost, "/api/v1/roles", tn.AccessToken, ports.CreateRoleRequest{Name: "Plain Role", HierarchyLevel: 60})
	if ok.status != http.StatusCreated {
		t.Fatalf("create a clean role: %d %s", ok.status, ok.body)
	}
	var created ports.CreateRoleResponse
	if err := json.Unmarshal(ok.body, &created); err != nil {
		t.Fatal(err)
	}
	r = f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+created.RoleID, tn.AccessToken, ports.UpdateRoleRequest{Name: "Plain" + rlo + "Role"})
	requireInvalidCharacters(t, "rename role", r, "role_invalid", "name")

	rolesAfter := f.authedJSON(t, http.MethodGet, "/api/v1/roles", tn.AccessToken, nil)
	var before, after ports.ListRolesResponse
	if json.Unmarshal(rolesBefore.body, &before) != nil || json.Unmarshal(rolesAfter.body, &after) != nil {
		t.Fatal("decode roles")
	}
	if len(after.Roles) != len(before.Roles)+1 { // only the clean role was written
		t.Errorf("roles %d -> %d; want +1 (the clean role only)", len(before.Roles), len(after.Roles))
	}
	for _, ro := range after.Roles {
		if ro.ID == created.RoleID && ro.Name != "Plain Role" {
			t.Errorf("the refused rename changed the role to %q", ro.Name)
		}
	}
}

func TestE2E_HumanText_FreeTextReasonsRefused(t *testing.T) {
	f := newE2EFixture(t)
	tn := f.registerAndLogin(t, "hreason")
	p := permission.IdentityPermissions
	opTok := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage, p.Platform.TenantsView, p.Platform.UsersManage}, false)

	// Impersonation reason (422 impersonation_invalid).
	r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", opTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tn.TenantID, Reason: "ticket 4711 " + rlo + "reordered", DurationMinutes: 30})
	requireInvalidCharacters(t, "impersonation reason", r, "impersonation_invalid", "reason")
	list := f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", opTok, nil)
	if got := jsonObject(t, list)["sessions"].([]any); len(got) != 0 {
		t.Errorf("a refused session was created: %s", list.body)
	}

	// Deactivating a member with a control character in the reason.
	m := f.createMember(t, tn, "hdeact")
	r = f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", tn.AccessToken,
		ports.DeactivateUserRequest{Reason: "left" + rlo + "company"})
	requireInvalidCharacters(t, "deactivate reason", r, "user_invalid", "reason")

	// Global suspension reason (422 person_invalid).
	r = f.authedJSON(t, http.MethodPost, "/api/v1/platform/persons/"+m.personID+"/global-suspend", opTok,
		ports.GlobalSuspendPersonRequest{Reason: "fraud\nclaim"})
	requireInvalidCharacters(t, "global-suspend reason", r, "person_invalid", "reason")
}

func TestE2E_HumanText_RegisterTenantNamesRefused_NothingWritten(t *testing.T) {
	f := newE2EFixture(t)
	op := f.mintPlatformTokenWith(t, nil, true)
	register := func(legal, display, first, last, slug string) httpResp {
		return f.authedJSON(t, http.MethodPost, "/api/v1/tenants", op, ports.RegisterTenantRequest{
			Slug: slug, LegalName: legal, DisplayName: display, AdminEmail: slug + "@e2e.test",
			AdminPassword: "Correct-Horse-Battery-9!x", AdminFirstName: first, AdminLastName: last,
		})
	}
	requireInvalidCharacters(t, "register display_name", register("Globex Pvt Ltd", "Glo"+rlo+"bex", "Ann", "Lee", "htext-a"), "tenant_invalid", "display_name")
	requireInvalidCharacters(t, "register legal_name", register("Globex\tPvt Ltd", "Globex", "Ann", "Lee", "htext-b"), "tenant_invalid", "legal_name")
	requireInvalidCharacters(t, "register admin first name", register("Globex Pvt Ltd", "Globex", "An"+rlo+"n", "Lee", "htext-c"), "person_invalid", "admin_first_name")
	requireInvalidCharacters(t, "register admin last name", register("Globex Pvt Ltd", "Globex", "Ann", "Le\x00e", "htext-d"), "person_invalid", "admin_last_name")
	for _, s := range []string{"htext-a", "htext-b", "htext-c", "htext-d"} {
		if got := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/by-slug/"+s, op, nil); got.status != http.StatusNotFound {
			t.Errorf("a refused registration left tenant %s behind: %d %s", s, got.status, got.body)
		}
	}
}
