//go:build integration

// arch-test:no-timeout-needed — newAttackWorld → newE2EFixtureWith →
// startWiredPostgresForHTTP uses context.WithTimeout(90s) internally; per-request
// HTTP uses t.Context().

// End-to-end tests for the identity contract gaps the web input audit found
// (branch fix/commerce-integrity, second round). One test per item group.

package main

import (
	"fmt"
	"net/http"
	"slices"
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
