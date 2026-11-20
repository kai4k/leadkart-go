//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goal 8: learn what one may not see from the DIFFERENCE between
// "does not exist" and "exists but not yours". For each pair of requests the
// whole response (status, error code, message) must be identical.

package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// shape is the part of a response an attacker can compare: status, code, message.
func shape(t *testing.T, r httpResp) string {
	t.Helper()
	var er ports.ErrorResponse
	if len(r.body) > 0 {
		er = decodeError(t, r.body)
	}
	return fmt.Sprintf("%d %s %q", r.status, er.Error, er.Message)
}

// sameShape fails when any two responses differ.
func sameShape(t *testing.T, what string, labelled map[string]httpResp) {
	t.Helper()
	var first, firstLabel string
	for label, r := range labelled {
		got := shape(t, r)
		if first == "" {
			first, firstLabel = got, label
			continue
		}
		if got != first {
			t.Errorf("%s: %q answers %s but %q answers %s — the difference is an oracle", what, label, got, firstLabel, first)
		}
	}
}

// (The nil UUID and a manager id equal to the subordinate are refused as malformed input,
// 400 / 422, before any lookup, for every caller alike; they say nothing about the data.)
//
// TestAttack_Oracles_ForeignAndMissingIdsAnswerIdentically is goal 8 for ids of
// other tenants: on every identity-side id route the answer for another tenant's
// id equals the answer for an id that exists nowhere (ADR 0044), for a caller
// with full authority in their own tenant.
func TestAttack_Oracles_ForeignAndMissingIdsAnswerIdentically(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	adm := w.tok(t, "admin")
	vRole := f.roleIDByName(t, w.victim.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	vReq := func() string {
		_, id := f.requestElevation(t, w.vExecToken(t), permission.IdentityPermissions.Users.View)
		return id
	}()
	missing := ids.NewV7().String()
	execRole := w.roles[role.SystemRoles.Tenant.SalesExecutive]
	for _, rt := range memberIDRoutes(execRole) {
		sameShape(t, "member route "+rt.what, map[string]httpResp{
			"another tenant's member": f.authedJSON(t, rt.method, fmt.Sprintf(rt.tmpl, w.vExec.membershipID), adm, rt.body),
			"a random id":             f.authedJSON(t, rt.method, fmt.Sprintf(rt.tmpl, missing), adm, rt.body),
		})
	}
	for _, rt := range roleIDRoutes() {
		sameShape(t, "role route "+rt.what, map[string]httpResp{
			"another tenant's role": f.authedJSON(t, rt.method, fmt.Sprintf(rt.tmpl, vRole), adm, rt.body),
			"a random id":           f.authedJSON(t, rt.method, fmt.Sprintf(rt.tmpl, missing), adm, rt.body),
		})
	}
	for _, c := range []struct{ what, method, tmpl string }{
		{"get member", http.MethodGet, "/api/v1/users/%s"}, {"get role", http.MethodGet, "/api/v1/roles/%s"},
	} {
		foreign := w.vExec.membershipID
		if c.what == "get role" {
			foreign = vRole
		}
		sameShape(t, c.what, map[string]httpResp{
			"foreign": f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, foreign), adm, nil),
			"missing": f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, missing), adm, nil),
		})
	}
	for _, c := range []struct {
		what, method, tmpl string
		body               any
	}{
		{"get request", http.MethodGet, "/api/v1/permission-requests/%s", nil},
		{"approve request", http.MethodPost, "/api/v1/permission-requests/%s/approve", ports.ApprovePermissionRequestRequest{DecisionReason: "oracle"}},
		{"deny request", http.MethodPost, "/api/v1/permission-requests/%s/deny", ports.DenyPermissionRequestRequest{DecisionReason: "oracle"}},
		{"cancel request", http.MethodPost, "/api/v1/permission-requests/%s/cancel", nil},
	} {
		sameShape(t, c.what+" (another tenant's request)", map[string]httpResp{
			"another tenant's request": f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, vReq), adm, c.body),
			"a random id":              f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, missing), adm, c.body),
		})
	}
	// and nothing the probes did changed either tenant
	if got := w.snapshotRequests(t); got != "" {
		t.Errorf("a probe left a pending request in the owner's queue: %s", got)
	}
}

// vExecToken logs the victim tenant's member in.
func (w *attackWorld) vExecToken(t *testing.T) string {
	t.Helper()
	return w.f.loginToken(t, w.vExec.email, w.vExec.password)
}

// TestAttack_Oracles_RequestsOfOthersAreIndistinguishableFromMissingOnes is goal 8
// for permission requests inside the tenant: a caller who may neither decide nor
// see a request gets the SAME answer as for a request that does not exist, on get,
// approve, deny and cancel, and whatever the request's state (pending, approved,
// denied, cancelled).
func TestAttack_Oracles_RequestsOfOthersAreIndistinguishableFromMissingOnes(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	pending := w.submitFor(t, "exec2", p.Inventory.Catalog.Manage)
	approved := w.submitFor(t, "exec2", p.Users.View)
	denied := w.submitFor(t, "exec2", p.Roles.View)
	cancelled := w.submitFor(t, "exec2", p.Crm.Leads.Read)
	if r := f.approveRequest(t, w.tok(t, "admin"), approved); r.status != http.StatusNoContent {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+denied+"/deny", w.tok(t, "admin"), ports.DenyPermissionRequestRequest{DecisionReason: "no"}); r.status != http.StatusNoContent {
		t.Fatalf("deny: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+cancelled+"/cancel", w.tok(t, "exec2"), nil); r.status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
	missing := ids.NewV7().String()
	calls := []struct {
		what, method, tmpl string
		body               any
	}{
		{"get", http.MethodGet, "/api/v1/permission-requests/%s", nil},
		{"approve", http.MethodPost, "/api/v1/permission-requests/%s/approve", ports.ApprovePermissionRequestRequest{DecisionReason: "oracle"}},
		{"deny", http.MethodPost, "/api/v1/permission-requests/%s/deny", ports.DenyPermissionRequestRequest{DecisionReason: "oracle"}},
		{"cancel", http.MethodPost, "/api/v1/permission-requests/%s/cancel", nil},
	}
	// callers who are neither the requester nor entitled to decide: peer, role-less, custom, a non-manager above the requester
	for _, who := range []string{"exec", "norole", "custom", "sales", "senior", "hr"} {
		for _, c := range calls {
			sameShape(t, who+" "+c.what, map[string]httpResp{
				"a missing request":   f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, missing), w.tok(t, who), c.body),
				"a pending request":   f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, pending), w.tok(t, who), c.body),
				"an approved request": f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, approved), w.tok(t, who), c.body),
				"a denied request":    f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, denied), w.tok(t, who), c.body),
				"a cancelled request": f.authedJSON(t, c.method, fmt.Sprintf(c.tmpl, cancelled), w.tok(t, who), c.body),
			})
		}
	}
	// the approval of exec2's request by the admin rotated exec2's stamp; nothing else may have moved
	if g := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+pending, w.owner.AccessToken, nil); g.status != http.StatusOK {
		t.Errorf("the pending request was disturbed: %d %s", g.status, g.body)
	}
}

// TestAttack_Oracles_ManagerRouteLeaksNothingAboutTheChain is goal 8 for the
// reporting chain: a caller who may not act on the subordinate is refused on that
// before the proposed manager is looked at, so every manager id, whether it
// names an active member, a more senior one, an inactive one, another tenant's
// member, or nothing at all, gets the same answer.
func TestAttack_Oracles_ManagerRouteLeaksNothingAboutTheChain(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	senior := w.mid(t, "senior") // "sales" does not outrank the Senior Manager
	for _, who := range []string{"sales", "hr", "lead", "exec"} {
		put := func(managerID string) httpResp {
			return f.authedJSON(t, http.MethodPut, "/api/v1/users/"+senior+"/manager", w.tok(t, who), ports.AssignUserManagerRequest{ManagerID: managerID})
		}
		sameShape(t, who+" sets the manager of a senior", map[string]httpResp{
			"a missing id":       put(ids.NewV7().String()),
			"an owner":           put(w.owner.MembershipID),
			"an administrator":   put(w.mid(t, "admin")),
			"an inactive member": put(w.gone.membershipID),
			"another tenant's":   put(w.vExec.membershipID),
			"themselves":         put(w.mid(t, who)),
			"a junior":           put(w.mid(t, "norole")),
			"another junior":     put(w.mid(t, "exec2")),
		})
		del := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+senior+"/manager", w.tok(t, who), nil)
		if del.status != http.StatusForbidden {
			t.Errorf("%s clears a senior's manager: %d %s, want 403", who, del.status, del.body)
		}
	}
}
