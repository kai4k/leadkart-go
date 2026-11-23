//go:build integration

// arch-test:raw-sql-justified — plants LEGACY stored values (a padded name, an over-cap profile field) that the routes now refuse to write, so no route or typed helper can produce them.
// arch-test:no-timeout-needed — newAttackWorld → newE2EFixtureWith →
// startWiredPostgresForHTTP uses context.WithTimeout(90s) internally; per-request
// HTTP uses t.Context().

// End-to-end tests for the identity contract gaps the web input audit found
// (branch fix/commerce-integrity, second round). One test per item group.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// overridesOfUser reads GET /users/{id} and returns the member's overrides, or nil
// when the caller was not shown any.
func (w *attackWorld) overridesFor(t *testing.T, tok, id string) *ports.UserPermissionOverridesDto {
	t.Helper()
	return w.f.getUser(t, tok, id).PermissionOverrides
}

// C1: the single read returns the overrides to the callers who may change them,
// a save that re-sends what the read returned changes nothing (a time-bound
// grant keeps its expiry and cannot be extended), and nobody else sees them.
func TestE2E_C1_UserReadReturnsOverridesAndASaveCannotErase(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	f := w.f
	// a member of its own: the world plants a stale tier-only grant on its personas, which a re-send is rightly refused
	cm := f.createMember(t, w.owner, "c1target")
	exec := cm.membershipID
	f.assignRole(t, w.owner.AccessToken, exec, w.roles[role.SystemRoles.Tenant.SalesExecutive])
	f.grantOverrides(t, w.owner.AccessToken, exec, atkRoutePerms(), nil)
	own := w.owner.AccessToken

	// a time-bound grant from an approved permission request, and a deliberate revocation
	req, reqID := f.requestElevation(t, f.loginToken(t, cm.email, cm.password), p.Inventory.Catalog.Manage)
	if req.status != http.StatusCreated {
		t.Fatalf("submit: %d %s", req.status, req.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+reqID+"/approve", own, ports.ApprovePermissionRequestRequest{DecisionReason: "c1"}); r.status != http.StatusNoContent {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}
	ov := w.overridesFor(t, own, exec)
	if ov == nil {
		t.Fatal("the owner is shown no overrides on the single read")
	}
	var timed *ports.GrantedOverrideDto
	for i := range ov.Granted {
		if ov.Granted[i].Name == p.Inventory.Catalog.Manage {
			timed = &ov.Granted[i]
		}
	}
	if timed == nil || timed.ExpiresAt.IsZero() || !timed.ExpiresAt.After(time.Now()) {
		t.Fatalf("the approved grant is %+v in %+v, want present with a future expires_at", timed, ov.Granted)
	}
	for _, perm := range atkRoutePerms() {
		if !slices.ContainsFunc(ov.Granted, func(g ports.GrantedOverrideDto) bool { return g.Name == perm && g.ExpiresAt.IsZero() }) {
			t.Errorf("the permanent grant %s is missing or time-bound: %+v", perm, ov.Granted)
		}
	}
	// a revocation joins the set
	names := func(o *ports.UserPermissionOverridesDto) []string {
		out := []string{}
		for _, g := range o.Granted {
			out = append(out, g.Name)
		}
		return out
	}
	path := "/api/v1/users/" + exec + "/permission-overrides"
	if r := f.authedJSON(t, http.MethodPatch, path, own, ports.ReplaceUserPermissionOverridesRequest{Granted: names(ov), Revoked: []string{p.Tasks.WorkItems.Manage}}); r.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	before := w.overridesOf(t, w.owner.TenantID, exec)
	ov = w.overridesFor(t, own, exec)
	if len(ov.Revoked) != 1 || ov.Revoked[0] != p.Tasks.WorkItems.Manage {
		t.Fatalf("revoked = %v, want [%s]", ov.Revoked, p.Tasks.WorkItems.Manage)
	}

	// the form's save: what the read returned, by name, plus fields the route does not have
	for _, body := range []any{
		ports.ReplaceUserPermissionOverridesRequest{Granted: names(ov), Revoked: ov.Revoked},
		rawBody(fmt.Sprintf(`{"granted":%s,"revoked":[%q],"overrides_version":%q,"expires_at":"2099-01-01T00:00:00Z"}`, jsonList(names(ov)), p.Tasks.WorkItems.Manage, f.overridesVersion(t, own, exec))),
	} {
		if r := f.authedJSON(t, http.MethodPatch, path, own, body); r.status != http.StatusNoContent {
			t.Fatalf("re-send: %d %s", r.status, r.body)
		}
		if after := w.overridesOf(t, w.owner.TenantID, exec); after != before {
			t.Fatalf("a save that re-sent what the read returned changed the overrides:\n before %s\n after  %s", before, after)
		}
	}
	// a name absent from the save is removed (the replace contract), and re-adding it
	// afterwards is a NEW permanent grant: the time-bound one cannot be rebuilt by hand
	rest := slices.DeleteFunc(names(ov), func(n string) bool { return n == p.Inventory.Catalog.Manage })
	if r := f.authedJSON(t, http.MethodPatch, path, own, ports.ReplaceUserPermissionOverridesRequest{Granted: rest, Revoked: ov.Revoked}); r.status != http.StatusNoContent {
		t.Fatalf("drop: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPatch, path, own, ports.ReplaceUserPermissionOverridesRequest{Granted: names(ov), Revoked: ov.Revoked}); r.status != http.StatusNoContent {
		t.Fatalf("re-add: %d %s", r.status, r.body)
	}
	re := w.overridesFor(t, own, exec)
	for _, g := range re.Granted {
		if g.Name == p.Inventory.Catalog.Manage && !g.ExpiresAt.IsZero() {
			t.Errorf("a re-added grant kept an expiry: %+v", g)
		}
	}
}

func jsonList(in []string) string {
	out := "["
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%q", s)
	}
	return out + "]"
}

// C1 visibility: only a caller who holds the permission AND may act on the member
// sees the field; everyone else gets the user without it, never an error.
func TestE2E_C1_OverridesAreShownOnlyToCallersWhoMayChangeThem(t *testing.T) {
	w := newAttackWorld(t)
	norole, exec, exec2 := w.mid(t, "norole"), w.mid(t, "exec"), w.mid(t, "exec2")
	rows := []struct {
		what, who, target string
		want              bool
	}{
		{"the owner reads a junior", "owner", norole, true},
		{"an administrator reads a junior", "admin", exec, true},
		{"a rank-50 holder of the permission reads a rank-99 member", "exec", norole, true},
		{"a peer of the same rank", "exec", exec2, false},
		{"a junior reads a senior", "norole", exec, false},
		{"a member reads their own", "norole", norole, false},
		{"the owner reads their own", "owner", w.owner.MembershipID, false},
		{"an administrator without update_permissions (tenant.admin revoked)", "adminRO", norole, false},
	}
	for _, r := range rows {
		got := w.overridesFor(t, w.tok(t, r.who), r.target)
		if (got != nil) != r.want {
			t.Errorf("%s: overrides shown = %v, want %v", r.what, got != nil, r.want)
		}
	}
	// another company: the member does not exist there (RLS), never a leak
	if resp := w.f.authedJSON(t, http.MethodGet, "/api/v1/users/"+norole, w.victim.AccessToken, nil); resp.status != http.StatusNotFound {
		t.Errorf("another tenant's owner reads this tenant's member: %d %s, want 404", resp.status, resp.body)
	}
	// and the list never carries them
	for _, u := range w.listUsers(t, w.owner.AccessToken) {
		if u.PermissionOverrides != nil {
			t.Errorf("the list shows the overrides of %s", u.MembershipID)
		}
	}
}

// C3: the member picker. Active members of the caller's own tenant, by name,
// for whoever may reassign leads; nothing but id, name and designation.
func TestE2E_C3_MemberSearch(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	search := func(tok, qs string) httpResp {
		return f.authedJSON(t, http.MethodGet, "/api/v1/users/search"+qs, tok, nil)
	}
	decode := func(r httpResp) ports.SearchMembersResponse {
		t.Helper()
		if r.status != http.StatusOK {
			t.Fatalf("search: %d %s", r.status, r.body)
		}
		var out ports.SearchMembersResponse
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("decode %s: %v", r.body, err)
		}
		return out
	}
	ids := func(o ports.SearchMembersResponse) []string {
		out := []string{}
		for _, m := range o.Members {
			out = append(out, m.MembershipID)
		}
		slices.Sort(out)
		return out
	}
	own := w.owner.AccessToken

	// by name, case-insensitive, substring; two members match "exec"
	got := decode(search(own, "?q=EXEC"))
	if want := []string{w.mid(t, "exec"), w.mid(t, "exec2")}; !slices.Equal(ids(got), func() []string { s := slices.Clone(want); slices.Sort(s); return s }()) {
		t.Errorf("q=EXEC returned %v, want %v", ids(got), want)
	}
	for _, m := range got.Members {
		if !strings.HasSuffix(m.DisplayName, " Member") {
			t.Errorf("display name %q is not 'first last'", m.DisplayName)
		}
	}
	// nothing but id, name, designation: no email, no phone, no person id
	raw := search(own, "?q=exec")
	var generic struct {
		Members []map[string]any `json:"members"`
	}
	if err := json.Unmarshal(raw.body, &generic); err != nil {
		t.Fatal(err)
	}
	for _, m := range generic.Members {
		for k := range m {
			if k != "membership_id" && k != "display_name" && k != "designation" {
				t.Errorf("the picker returns the field %q", k)
			}
		}
	}
	if strings.Contains(string(raw.body), "@") || strings.Contains(string(raw.body), w.who["exec"].personID) {
		t.Errorf("the picker leaks an email or a person id: %s", raw.body)
	}
	// the email is not searchable (it would leak a mailbox through a name box)
	if m := decode(search(own, "?q=e2e.test")).Members; len(m) != 0 {
		t.Errorf("a search by email domain returned %d members", len(m))
	}
	// a deactivated member is not offered; another tenant's member is not found
	if m := decode(search(own, "?q=gone")).Members; len(m) != 0 {
		t.Errorf("a deactivated member was offered: %+v", m)
	}
	if m := decode(search(own, "?q=vexec")).Members; len(m) != 0 {
		t.Errorf("another company's member was offered: %+v", m)
	}
	if m := decode(search(w.victim.AccessToken, "?q=exec")).Members; len(m) != 1 || m[0].MembershipID != w.vExec.membershipID {
		t.Errorf("the other company's owner got %+v, want only their own vexec", m)
	}
	// LIKE wildcards are plain text
	for _, q := range []string{"%25%25", "_x", "e_ec", "ex%25"} {
		if m := decode(search(own, "?q="+q)).Members; len(m) != 0 {
			t.Errorf("q=%s matched %+v: a wildcard was interpreted", q, m)
		}
	}
	// bounds: 2-100 characters after trimming, limit up to 20
	if r := search(own, "?q=a"); r.status != http.StatusBadRequest || errCodeOf(r) != "search_query_too_short" {
		t.Errorf("one character: %d %s", r.status, r.body)
	}
	if r := search(own, "?q=%20%20a%20%20"); r.status != http.StatusBadRequest || errCodeOf(r) != "search_query_too_short" {
		t.Errorf("one character padded: %d %s", r.status, r.body)
	}
	if r := search(own, "?q="+strings.Repeat("a", 101)); r.status != http.StatusBadRequest || errCodeOf(r) != "search_query_too_long" {
		t.Errorf("101 characters: %d %s", r.status, r.body)
	}
	if r := search(own, ""); r.status != http.StatusBadRequest {
		t.Errorf("no q: %d %s", r.status, r.body)
	}
	if n := len(decode(search(own, "?q=member&limit=1")).Members); n != 1 {
		t.Errorf("limit=1 returned %d", n)
	}
	if n := len(decode(search(own, "?q=member&limit=500")).Members); n > 20 {
		t.Errorf("limit=500 returned %d, want at most 20", n)
	}
	// who may call it: whoever may reassign leads (crm.leads.assign)
	if r := search(w.tok(t, "sales"), "?q=exec"); r.status != http.StatusOK {
		t.Errorf("a Sales Manager (may assign leads): %d %s, want 200", r.status, r.body)
	}
	if r := search(w.tok(t, "exec"), "?q=exec"); r.status != http.StatusForbidden {
		t.Errorf("a Sales Executive (may not assign leads): %d %s, want 403", r.status, r.body)
	}
	if r := search("", "?q=exec"); r.status != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", r.status)
	}
	// a literal route does not shadow a member whose id is a UUID
	if u := f.getUser(t, own, w.mid(t, "exec")); u.MembershipID != w.mid(t, "exec") {
		t.Errorf("GET /users/{id} broke: %+v", u)
	}
}

// C4: a deactivated member can be listed (?status=inactive) and so found again;
// the default listing is unchanged; the first page carries the chip counts.
func TestE2E_C4_UsersListStatusFilter(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	own := w.owner.AccessToken
	list := func(tok, qs string) (ports.ListUsersResponse, httpResp) {
		r := f.authedJSON(t, http.MethodGet, "/api/v1/users"+qs, tok, nil)
		var out ports.ListUsersResponse
		if r.status == http.StatusOK {
			if err := json.Unmarshal(r.body, &out); err != nil {
				t.Fatalf("decode %s: %v", r.body, err)
			}
		}
		return out, r
	}
	idsOf := func(o ports.ListUsersResponse) []string {
		out := []string{}
		for _, u := range o.Users {
			out = append(out, u.MembershipID)
		}
		slices.Sort(out)
		return out
	}
	sorted := func(in ...string) []string { slices.Sort(in); return in }

	def, _ := list(own, "?page_size=200")
	act, _ := list(own, "?status=active&page_size=200")
	if !slices.Equal(idsOf(def), idsOf(act)) {
		t.Fatalf("the default listing differs from ?status=active")
	}
	for _, u := range def.Users {
		if u.Status != "active" {
			t.Errorf("the default listing holds %s (%s)", u.MembershipID, u.Status)
		}
	}
	inact, _ := list(own, "?status=inactive&page_size=200")
	if want := sorted(w.gone.membershipID, w.goneOwner.membershipID); !slices.Equal(idsOf(inact), want) {
		t.Errorf("inactive = %v, want %v", idsOf(inact), want)
	}
	for _, u := range inact.Users {
		if u.Status != "inactive" || u.PermissionOverrides != nil {
			t.Errorf("inactive row %+v", u)
		}
	}
	all, _ := list(own, "?status=all&page_size=200")
	if want := sorted(append(idsOf(def), w.gone.membershipID, w.goneOwner.membershipID)...); !slices.Equal(idsOf(all), want) {
		t.Errorf("all = %v, want %v", idsOf(all), want)
	}
	// counts: first page only, the whole tenant whatever the filter
	for _, qs := range []string{"?page_size=200", "?status=inactive&page_size=1", "?status=all&page_size=200"} {
		got, _ := list(own, qs)
		if got.StatusCounts == nil || got.StatusCounts.Active != len(def.Users) || got.StatusCounts.Inactive != 2 {
			t.Errorf("%s: status_counts = %+v, want active %d inactive 2", qs, got.StatusCounts, len(def.Users))
		}
	}
	first, _ := list(own, "?status=all&page_size=2")
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("page 1 has no next cursor: %+v", first)
	}
	second, _ := list(own, "?status=all&page_size=200&cursor="+first.NextCursor)
	if second.StatusCounts != nil {
		t.Errorf("a later page repeats the counts: %+v", second.StatusCounts)
	}
	if len(second.Users)+len(first.Users) != len(all.Users) {
		t.Errorf("paging lost rows: %d + %d != %d", len(first.Users), len(second.Users), len(all.Users))
	}
	// a bad filter, and another company never sees these rows
	if _, r := list(own, "?status=deleted"); r.status != http.StatusBadRequest || errCodeOf(r) != "invalid_status_filter" {
		t.Errorf("?status=deleted: %d %s", r.status, r.body)
	}
	if v, _ := list(w.victim.AccessToken, "?status=all&page_size=200"); len(v.Users) != 2 || v.StatusCounts.Inactive != 0 {
		t.Errorf("the other company sees %d users, counts %+v", len(v.Users), v.StatusCounts)
	}
	// the point of the filter: the member can be found and reactivated, then listed as active
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+w.gone.membershipID+"/reactivate", own, nil); r.status != http.StatusNoContent {
		t.Fatalf("reactivate: %d %s", r.status, r.body)
	}
	inact, _ = list(own, "?status=inactive&page_size=200")
	back, _ := list(own, "?page_size=200")
	if slices.Contains(idsOf(inact), w.gone.membershipID) || !slices.Contains(idsOf(back), w.gone.membershipID) || back.StatusCounts.Inactive != 1 {
		t.Errorf("after reactivation: inactive %v, counts %+v", idsOf(inact), back.StatusCounts)
	}
}

// familyOf reads the family_id claim of an access token (the session it belongs to).
func familyOf(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var c struct {
		FamilyID string `json:"family_id"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return c.FamilyID
}

// I1: "sign out other devices" keeps the device that asked.
func TestE2E_I1_SignOutOtherDevicesKeepsThisOne(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "i1")
	m := f.createMember(t, owner, "i1dev")
	phone, laptop, tablet := f.loginToken(t, m.email, m.password), f.loginToken(t, m.email, m.password), f.loginToken(t, m.email, m.password)
	famPhone, famLaptop, famTablet := familyOf(t, phone), familyOf(t, laptop), familyOf(t, tablet)
	if famPhone == "" || famPhone == famLaptop || famLaptop == famTablet {
		t.Fatalf("each login must carry its own family_id, got %q %q %q", famPhone, famLaptop, famTablet)
	}
	if got := f.sessionIDs(t, phone); len(got) != 3 {
		t.Fatalf("sessions before = %v, want 3", got)
	}

	// a body that does not parse must not read as "everything"
	if r := f.authedJSON(t, http.MethodDelete, "/api/v1/auth/sessions", phone, rawBody(`{"except_current":"true"}`)); r.status != http.StatusBadRequest {
		t.Fatalf("a malformed body: %d %s, want 400", r.status, r.body)
	}
	if got := f.sessionIDs(t, phone); len(got) != 3 {
		t.Fatalf("a refused call revoked sessions: %v", got)
	}

	r := f.authedJSON(t, http.MethodDelete, "/api/v1/auth/sessions", phone, ports.RevokeAllSessionsRequest{ExceptCurrent: true})
	var out ports.RevokeAllSessionsResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil || out.RevokedCount != 2 {
		t.Fatalf("except current: %d %s, want 200 revoked_count 2 (the kept session is not counted)", r.status, r.body)
	}
	got := f.sessionIDs(t, phone)
	if len(got) != 1 || got[0] != famPhone {
		t.Fatalf("sessions after = %v, want only this device's %s", got, famPhone)
	}
	// without the flag it is everything, this device included (unchanged behaviour)
	r = f.authedJSON(t, http.MethodDelete, "/api/v1/auth/sessions", phone, nil)
	if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil || out.RevokedCount != 1 {
		t.Fatalf("revoke all: %d %s, want 200 revoked_count 1", r.status, r.body)
	}
	if got := f.sessionIDs(t, phone); len(got) != 0 {
		t.Fatalf("sessions after revoke-all = %v, want none", got)
	}
}

// A refreshed token names the same session.
func TestE2E_I1_RefreshKeepsTheFamilyClaim(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "i1r")
	before := familyOf(t, owner.AccessToken)
	if before == "" {
		t.Fatal("the login token names no session")
	}
	status, lr := f.mustRefresh(t, owner.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("refresh: %d", status)
	}
	if after := familyOf(t, lr.AccessToken); after != before {
		t.Errorf("family after refresh = %q, want %q", after, before)
	}
}

// I7: a blank audit reason opens nothing; a padded one is stored trimmed.
func TestE2E_I7_ImpersonationReasonIsMeasuredAfterTrimming(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "i7")
	platformTok := f.mintPlatformToken(t, "")
	open := func(reason string) httpResp {
		return f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", platformTok,
			ports.CreateImpersonationSessionRequest{TargetTenantID: tenantA.TenantID, Reason: reason})
	}
	for _, blank := range []string{strings.Repeat(" ", 10), strings.Repeat(" ", 50), "   short    "} {
		if r := open(blank); r.status != http.StatusUnprocessableEntity || errCodeOf(r) != ports.ErrCodeImpersonationInvalid {
			t.Errorf("reason %q: %d %s, want 422 impersonation_invalid", blank, r.status, r.body)
		}
	}
	list := f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", platformTok, nil)
	var sessions ports.ListImpersonationSessionsResponse
	if err := json.Unmarshal(list.body, &sessions); err != nil || len(sessions.Sessions) != 0 {
		t.Fatalf("a refused reason opened a session: %s (%v)", list.body, err)
	}
	if r := open("   investigating ticket 4711   "); r.status != http.StatusCreated {
		t.Fatalf("a padded reason of real length: %d %s, want 201", r.status, r.body)
	}
	list = f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", platformTok, nil)
	if err := json.Unmarshal(list.body, &sessions); err != nil || len(sessions.Sessions) != 1 || sessions.Sessions[0].Reason != "investigating ticket 4711" {
		t.Fatalf("the stored reason is not trimmed: %s (%v)", list.body, err)
	}
}

// I5: names are trimmed before they are measured and stored trimmed; a full
// replace that re-sends an unchanged stored value (even one stored with padding
// before this rule) stays accepted.
func TestE2E_I5_NamesAreStoredTrimmed(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "i5")

	// a person created with padded names
	suffix := ids.NewV7().String()
	suffix = suffix[len(suffix)-8:]
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken, ports.CreateUserRequest{
		Email: "i5-" + suffix + "@e2e.test", Password: "Member-Pass-2026!" + suffix, FirstName: "  Ravi ", LastName: "\tKumar  "})
	if resp.status != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.status, resp.body)
	}
	var cu ports.CreateUserResponse
	if err := json.Unmarshal(resp.body, &cu); err != nil {
		t.Fatal(err)
	}
	if u := f.getUser(t, owner.AccessToken, cu.MembershipID); u.FirstName != "Ravi" || u.LastName != "Kumar" {
		t.Errorf("stored names = %q %q, want them trimmed", u.FirstName, u.LastName)
	}
	// 100 characters of name plus padding fits (the limit is measured after trimming)
	long := strings.Repeat("n", 100)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken, ports.CreateUserRequest{
		Email: "i5b-" + suffix + "@e2e.test", Password: "Member-Pass-2026!" + suffix, FirstName: "   " + long + "   ", LastName: "K"}); r.status != http.StatusCreated {
		t.Errorf("100 characters padded with spaces: %d %s, want 201", r.status, r.body)
	}
	// a blank name is still refused
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken, ports.CreateUserRequest{
		Email: "i5c-" + suffix + "@e2e.test", Password: "Member-Pass-2026!" + suffix, FirstName: "    ", LastName: "K"}); r.status != http.StatusUnprocessableEntity {
		t.Errorf("a blank first name: %d %s, want 422", r.status, r.body)
	}

	// the company: a padded rename is stored trimmed
	path := "/api/v1/tenants/" + owner.TenantID + "/profile"
	if r := f.authedJSON(t, http.MethodPatch, path, owner.AccessToken, ports.UpdateTenantProfileRequest{LegalName: "  Padded Legal Pvt Ltd ", DisplayName: " Padded "}); r.status != http.StatusNoContent && r.status != http.StatusOK {
		t.Fatalf("rename: %d %s", r.status, r.body)
	}
	tenantNames := func() (string, string) {
		r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/"+owner.TenantID, owner.AccessToken, nil)
		var tn ports.TenantDto
		if err := json.Unmarshal(r.body, &tn); err != nil || r.status != http.StatusOK {
			t.Fatalf("get tenant: %d %s (%v)", r.status, r.body, err)
		}
		return tn.LegalName, tn.DisplayName
	}
	if l, d := tenantNames(); l != "Padded Legal Pvt Ltd" || d != "Padded" {
		t.Errorf("stored company names = %q / %q, want them trimmed", l, d)
	}
	// a company stored with padding (before the rule) accepts its own values back, untouched
	if _, err := f.Pool.Exec(t.Context(), `UPDATE identity.tenants SET legal_name = ' Legacy Legal ', display_name = ' Legacy ' WHERE id = $1`, owner.TenantID); err != nil {
		t.Fatalf("plant legacy names: %v", err)
	}
	if r := f.authedJSON(t, http.MethodPatch, path, owner.AccessToken, ports.UpdateTenantProfileRequest{LegalName: " Legacy Legal ", DisplayName: " Legacy "}); r.status != http.StatusNoContent && r.status != http.StatusOK {
		t.Errorf("re-sending the stored, padded values: %d %s, want it accepted", r.status, r.body)
	}
	if l, d := tenantNames(); l != " Legacy Legal " || d != " Legacy " {
		t.Errorf("an untouched save rewrote the stored names: %q / %q", l, d)
	}
}

// C7 + I6: invalid_slug and invalid_email name their field (the status and the
// code are unchanged).
func TestE2E_C7_I6_InvalidEmailNamesItsField(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "i6")
	decodeErr := func(r httpResp) ports.ErrorResponse {
		t.Helper()
		var er ports.ErrorResponse
		if err := json.Unmarshal(r.body, &er); err != nil {
			t.Fatalf("decode %s: %v", r.body, err)
		}
		return er
	}
	want := func(what string, r httpResp, field string) {
		t.Helper()
		er := decodeErr(r)
		if r.status != http.StatusBadRequest || er.Error != "invalid_email" || len(er.Errors[field]) != 1 || er.Errors[field][0] != "invalid_email" {
			t.Errorf("%s: %d %s, want 400 invalid_email with errors{%s:[invalid_email]}", what, r.status, r.body, field)
		}
	}
	for _, bad := range []string{"a..b@x.com", ".a@x.com", "a,b@x.com", "not-an-email"} {
		want("add member "+bad, f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken,
			ports.CreateUserRequest{Email: bad, Password: "Member-Pass-2026!x", FirstName: "A", LastName: "B"}), "email")
		want("change email "+bad, f.authedJSON(t, http.MethodPost, "/api/v1/auth/request-email-change", owner.AccessToken,
			ports.RequestEmailChangeRequest{NewEmail: bad}), "new_email")
	}
}

// M1: the profile fields have a server cap (designation and department 120, status
// message 280, UTF-8 bytes after trimming): too_long, named, nothing written.
func TestE2E_M1_ProfileFieldsAreCapped(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "m1")
	m := f.createMember(t, owner, "m1member")
	path := "/api/v1/users/" + m.membershipID + "/profile"
	save := func(d, dep, sm string) httpResp {
		return f.authedJSON(t, http.MethodPatch, path, owner.AccessToken, ports.UpdateUserProfileRequest{Designation: d, Department: dep, StatusMessage: sm})
	}
	if r := save("  "+strings.Repeat("d", 120)+"  ", strings.Repeat("p", 120), strings.Repeat("s", 280)); r.status != http.StatusNoContent {
		t.Fatalf("every field at its cap (one padded with spaces): %d %s, want 204", r.status, r.body)
	}
	for _, c := range []struct{ field, d, dep, sm string }{
		{"designation", strings.Repeat("d", 121), "", ""},
		{"department", "", strings.Repeat("p", 121), ""},
		{"status_message", "", "", strings.Repeat("s", 281)},
		{"designation", strings.Repeat("é", 61), "", ""}, // 122 bytes
	} {
		r := save(c.d, c.dep, c.sm)
		var er ports.ErrorResponse
		if err := json.Unmarshal(r.body, &er); err != nil || r.status != http.StatusUnprocessableEntity || er.Error != "user_invalid" || len(er.Errors[c.field]) != 1 || er.Errors[c.field][0] != "too_long" {
			t.Errorf("%s over its cap: %d %s, want 422 user_invalid errors{%s:[too_long]}", c.field, r.status, r.body, c.field)
		}
	}
	u := f.getUser(t, owner.AccessToken, m.membershipID)
	if u.Designation != strings.Repeat("d", 120) || u.StatusMessage != strings.Repeat("s", 280) {
		t.Errorf("a refused save changed the stored profile: %q / %q", u.Designation, u.StatusMessage)
	}
	// a stored value over the cap (from before the cap) is accepted when re-sent unchanged
	long := strings.Repeat("L", 200)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.is_platform', 'true', true)`); err != nil {
		t.Fatalf("platform scope: %v", err)
	}
	if tag, err := tx.Exec(t.Context(), `UPDATE identity.tenant_memberships SET designation = $1 WHERE id = $2`, long, m.membershipID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("plant legacy value: %v (rows %d)", err, tag.RowsAffected())
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if r := save(long, strings.Repeat("p", 120), strings.Repeat("s", 280)); r.status != http.StatusNoContent {
		t.Errorf("re-sending a stored overlong value unchanged: %d %s, want 204", r.status, r.body)
	}
}

// Item 9: a GSTIN whose embedded PAN is not the supplied PAN names the field
// (it used to be a field-less 422 tenant_invalid), and nothing is written.
func TestE2E_GSTPANMismatchNamesItsField(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "gstpan")
	path := "/api/v1/tenants/" + owner.TenantID + "/statutory"
	save := func(gstin, pan string) httpResp {
		return f.authedJSON(t, http.MethodPatch, path, owner.AccessToken,
			map[string]any{"gst_number": gstin, "pan_number": pan, "drug_licence_number": ""})
	}
	if r := save("29ABCPE1234F1Z5", "ABCPE1234F"); r.status != http.StatusNoContent {
		t.Fatalf("matching GSTIN and PAN: %d %s, want 204", r.status, r.body)
	}
	r := save("29ABCPE1234F1Z5", "ZZZPZ9999G")
	var er ports.ErrorResponse
	if err := json.Unmarshal(r.body, &er); err != nil || r.status != http.StatusUnprocessableEntity || er.Error != "tenant_invalid" ||
		len(er.Errors) != 1 || len(er.Errors["pan_number"]) != 1 || er.Errors["pan_number"][0] != "gstin_pan_mismatch" {
		t.Fatalf("mismatch: %d %s, want 422 tenant_invalid errors{pan_number:[gstin_pan_mismatch]}", r.status, r.body)
	}
	if strings.Contains(string(r.body), "ZZZPZ9999G") || strings.Contains(string(r.body), "ABCPE1234F") {
		t.Errorf("the refusal echoes a statutory number: %s", r.body)
	}
	// a GSTIN alone, or a PAN alone, is not a mismatch
	if r := save("29ABCPE1234F1Z5", ""); r.status != http.StatusNoContent {
		t.Errorf("GSTIN without a PAN: %d %s, want 204", r.status, r.body)
	}
	if r := save("", "ZZZPZ9999G"); r.status != http.StatusNoContent {
		t.Errorf("PAN without a GSTIN: %d %s, want 204", r.status, r.body)
	}
}

// Identity fix round 2 (M1, M2): bytes a database cannot store in a query string are
// a 400 on every route. This runs behind the REAL public chain (buildPublicChain):
// the refusal is a chain middleware, not a handler's job.
func TestE2E_FixRound2_BadBytesInAQueryAre400OnEveryRoute(t *testing.T) {
	f, _ := newE2EChainFixture(t, nil)
	own := f.registerAndLogin(t, "badq").AccessToken
	for _, route := range []string{"/api/v1/users/search?q=", "/api/v1/inventory/products?search=", "/api/v1/users?cursor=", "/api/v1/roles?x="} {
		for name, v := range map[string]string{"NUL at the start": "%00ab", "NUL at the end": "ab%00", "invalid UTF-8": "%ff%fe", "a truncated sequence": "ex%c3"} {
			r := f.authedJSON(t, http.MethodGet, route+v, own, nil)
			if r.status != http.StatusBadRequest || errorCode(t, r.body) != "invalid_query" {
				t.Errorf("%s%s (%s): %d %s, want 400 invalid_query", route, v, name, r.status, r.body)
			}
		}
	}
	// readable queries are untouched, Hindi included
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users/search?q=%E0%A4%B0%E0%A4%BE%E0%A4%AE", own, nil); r.status != http.StatusOK {
		t.Errorf("a Hindi search: %d %s, want 200", r.status, r.body)
	}
	// an anonymous route is covered too: the refusal comes before authentication
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/check-slug?slug=%00", "", nil); r.status != http.StatusBadRequest || errorCode(t, r.body) != "invalid_query" {
		t.Errorf("an anonymous route with NUL: %d %s, want 400 invalid_query", r.status, r.body)
	}
}

// Identity fix round 2 (M3, M5): a character limit counts characters, and a misspelt
// key in an optional body is refused.
func TestE2E_FixRound2_CharacterLimitsAndStrictOptionalBodies(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	own := w.owner.AccessToken

	// M3: a 7-character Hindi reason (21 bytes, under the 10-character floor) is a
	// refusal, not a 500 from the column's CHECK; 10 characters (30 bytes) is accepted.
	p := permission.IdentityPermissions
	short := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", w.tok(t, "exec"), ports.CreatePermissionRequestRequest{Permission: p.Users.View, Reason: strings.Repeat("अ", 7)})
	if short.status < 400 || short.status >= 500 {
		t.Errorf("a 7-character Hindi reason: %d %s, want a 4xx refusal", short.status, short.body)
	}
	ok := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", w.tok(t, "exec"), ports.CreatePermissionRequestRequest{Permission: p.Users.View, Reason: strings.Repeat("अ", 10)})
	if ok.status != http.StatusCreated {
		t.Fatalf("a 10-character Hindi reason: %d %s, want 201", ok.status, ok.body)
	}

	// M5: a misspelt key in an optional body is refused, and nothing happens.
	var cr ports.CreatePermissionRequestResponse
	if err := json.Unmarshal(ok.body, &cr); err != nil {
		t.Fatal(err)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+cr.RequestID+"/approve", own, rawBody(`{"decisionReason":"fine"}`)); r.status != http.StatusBadRequest || errorCode(t, r.body) != "invalid_body" {
		t.Errorf("approve with a misspelt key: %d %s, want 400 invalid_body", r.status, r.body)
	}
	g := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+cr.RequestID, own, nil)
	var d ports.PermissionRequestDto
	if err := json.Unmarshal(g.body, &d); err != nil || d.State != "pending" {
		t.Errorf("after the refused approve the request is %q (%v), want pending", d.State, err)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+cr.RequestID+"/approve", own, nil); r.status != http.StatusNoContent && r.status != http.StatusOK {
		t.Errorf("approve with no body: %d %s, want it to succeed", r.status, r.body)
	}
}
