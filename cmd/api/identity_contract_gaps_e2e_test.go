//go:build integration

// arch-test:no-timeout-needed — newAttackWorld → newE2EFixtureWith →
// startWiredPostgresForHTTP uses context.WithTimeout(90s) internally; per-request
// HTTP uses t.Context().

// End-to-end tests for the identity contract gaps the web input audit found
// (branch fix/commerce-integrity, second round). One test per item group.

package main

import (
	"encoding/json"
	"fmt"
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
		rawBody(fmt.Sprintf(`{"granted":%s,"revoked":[%q],"expires_at":"2099-01-01T00:00:00Z"}`, jsonList(names(ov)), p.Tasks.WorkItems.Manage)),
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
