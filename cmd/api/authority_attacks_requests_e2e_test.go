//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goals 1, 4 and 8 through the permission-request routes: obtain
// tenant.admin (or any permission one may not hold) by asking for it, by having
// someone approve it, or by approving it oneself.

package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// submitFor submits a request as a persona and returns its id; it fails the test
// when the request is refused.
func (w *attackWorld) submitFor(t *testing.T, who, perm string) string {
	t.Helper()
	r, id := w.f.requestElevation(t, w.tok(t, who), perm)
	if r.status != http.StatusCreated {
		t.Fatalf("%s submits %s: %d %s", who, perm, r.status, r.body)
	}
	w.watched = append(w.watched, id)
	return id
}

// TestAttack_Requests_NothingUnrequestableIsEverSubmitted is goal 1 through the
// submit route: tenant.admin, every platform-reserved name, unknown and odd names
// are refused for every persona, and no request row appears (the owner's queue
// is part of the snapshot). A member with no role cannot ask at all.
func TestAttack_Requests_NothingUnrequestableIsEverSubmitted(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	names := []string{p.Meta.TenantAdmin, p.Platform.TenantsView, p.Platform.TenantsManage, p.Platform.UsersManage, p.Platform.RolesManage,
		p.Platform.ImpersonationManage, p.Platform.TenantsOwnership, p.Tenants.Suspend, p.Tenants.Delete, p.Users.Anonymise, p.PlatformLeadCredits.Topup}
	var rows []attack
	for _, actor := range []string{"admin", "office", "adminRO", "senior", "sales", "hr", "lead", "exec", "exec2"} {
		for _, name := range names {
			rows = append(rows, attack{who: actor, what: "request " + name, method: http.MethodPost, path: "/api/v1/permission-requests",
				body:   ports.CreatePermissionRequestRequest{Permission: name, Reason: elevationReason},
				status: http.StatusUnprocessableEntity, code: "permission_not_requestable"})
		}
		for _, bad := range []string{"", "no.such.permission", "TENANT.ADMIN"} {
			rows = append(rows, attack{who: actor, what: fmt.Sprintf("request %q", bad), method: http.MethodPost, path: "/api/v1/permission-requests",
				body: ports.CreatePermissionRequestRequest{Permission: bad, Reason: elevationReason}, status: http.StatusUnprocessableEntity})
		}
		for _, days := range []int{-1, 100000} {
			rows = append(rows, attack{who: actor, what: fmt.Sprintf("request for %d days", days), method: http.MethodPost, path: "/api/v1/permission-requests",
				body: ports.CreatePermissionRequestRequest{Permission: p.Inventory.Catalog.Manage, DurationDays: days, Reason: elevationReason}, status: http.StatusUnprocessableEntity})
		}
	}
	// no role, no identity.meta.request_permission_elevation
	for _, nobody := range []string{"norole", "custom"} {
		rows = append(rows, attack{who: nobody, what: "request anything", method: http.MethodPost, path: "/api/v1/permission-requests",
			body: ports.CreatePermissionRequestRequest{Permission: p.Inventory.Catalog.Manage, Reason: elevationReason}, status: http.StatusForbidden, code: "forbidden"})
	}
	w.assertGrouped(t, rows)
}

// TestAttack_Requests_ApprovalIsTheCeilingOfTheApprover is goals 1 and 4 through
// approve and deny: exec2 (a Sales Executive whose manager is the Team Lead) asks
// for two requestable names. Everyone who is not entitled to decide is told the
// request does not exist; the one entitled approver who does not hold the name is
// refused on the ceiling; the requester cannot decide their own request; and none
// of it changes the request or exec2's permissions.
func TestAttack_Requests_ApprovalIsTheCeilingOfTheApprover(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	notHeld := w.submitFor(t, "exec2", p.Inventory.Catalog.Manage) // the Team Lead does not hold it
	mustHold := w.submitFor(t, "exec2", p.Users.Deactivate)        // an Administrator must HOLD it to approve it
	decide := func(verb, id string) (string, any) {
		path := "/api/v1/permission-requests/" + id + "/" + verb
		if verb == "deny" {
			return path, ports.DenyPermissionRequestRequest{DecisionReason: "attack"}
		}
		return path, ports.ApprovePermissionRequestRequest{DecisionReason: "attack"}
	}
	var rows []attack
	for _, verb := range []string{"approve", "deny"} {
		path, body := decide(verb, notHeld)
		for _, who := range []string{"exec", "norole", "custom", "sales", "hr", "senior", "exec2"} {
			status, code := http.StatusNotFound, "permission_request_not_found"
			if who == "exec2" {
				status, code = http.StatusForbidden, "self_modification_forbidden"
			}
			rows = append(rows, attack{who: who, what: verb + " a request they may not decide", method: http.MethodPost, path: path, body: body, status: status, code: code})
		}
		// the victim tenant's owner cannot even see it
		rows = append(rows, attack{tok: w.victim.AccessToken, who: "victim-owner", what: verb + " across tenants", method: http.MethodPost, path: path, body: body,
			status: http.StatusNotFound, code: "permission_request_not_found"})
	}
	// the requester's manager decides within her own authority: she does not hold the name.
	path, body := decide("approve", notHeld)
	rows = append(rows, attack{who: "lead", what: "approve a name she does not hold", method: http.MethodPost, path: path, body: body,
		status: http.StatusForbidden, code: "grant_exceeds_authority"})
	// an Administrator without tenant.admin does not hold users.deactivate (must-hold set).
	path, body = decide("approve", mustHold)
	rows = append(rows, attack{who: "adminRO", what: "approve a must-hold name without holding it", method: http.MethodPost, path: path, body: body,
		status: http.StatusForbidden, code: "grant_exceeds_authority"})
	w.assertGrouped(t, rows)
}
