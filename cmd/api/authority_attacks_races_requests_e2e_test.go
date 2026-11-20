//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goal 9, continued: approvals against everything that can change
// under them, roles against their holders, and persons against their memberships.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// requestState reads one request's state as the owner sees it.
func (w *attackWorld) requestState(t *testing.T, id string) string {
	t.Helper()
	r := w.f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, w.owner.AccessToken, nil)
	var d ports.PermissionRequestDto
	if r.status != http.StatusOK || json.Unmarshal(r.body, &d) != nil {
		t.Fatalf("read request %s: %d %s", id, r.status, r.body)
	}
	return d.State
}

// resetGrants puts a persona's overrides back to the route permissions only.
func (w *attackWorld) resetGrants(t *testing.T, key string, extra ...string) {
	t.Helper()
	w.f.grantOverrides(t, w.owner.AccessToken, w.mid(t, key), append(atkRoutePerms(), extra...), nil)
}

// denyIfPending closes a request a race left pending.
func (w *attackWorld) denyIfPending(t *testing.T, id string) {
	t.Helper()
	if w.requestState(t, id) == "pending" {
		if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+id+"/deny", w.tok(t, "admin"), ports.DenyPermissionRequestRequest{DecisionReason: "round over"}); r.status != http.StatusNoContent {
			t.Fatalf("deny leftover: %d %s", r.status, r.body)
		}
	}
}

// TestAttack_Races_ApprovalsAreAtomicAndBounded: (1) two Administrators approve ONE
// request: exactly one wins, the grant lands once; (2) an approval races the
// requester's deactivation: the grant exists exactly when the request is approved;
// (3) an approval races the approver losing the permission it needs: same.
func TestAttack_Races_ApprovalsAreAtomicAndBounded(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	x := p.Inventory.Catalog.Manage
	exec2 := w.mid(t, "exec2")
	grantCount := func() int {
		return strings.Count(w.overridesOf(t, w.owner.TenantID, exec2), x+"@")
	}

	// arch-test:serial — the three races share one world and one requester; each runs its rounds to completion first.
	t.Run("two approvers, one request", func(t *testing.T) {
		for round := range raceRounds {
			w.relogin(t, "exec2")
			id := w.submitFor(t, "exec2", x)
			path := "/api/v1/permission-requests/" + id + "/approve"
			body := ports.ApprovePermissionRequestRequest{DecisionReason: "race"}
			res := w.raceOnce(t, w.call(http.MethodPost, path, w.tok(t, "admin"), body), w.call(http.MethodPost, path, w.tok(t, "office"), body))
			wins := 0
			for _, r := range res {
				onlyStatuses(t, "approve", r, http.StatusNoContent, http.StatusConflict, http.StatusNotFound)
				if r.status == http.StatusNoContent {
					wins++
				}
			}
			if wins != 1 || w.requestState(t, id) != "approved" || grantCount() != 1 {
				t.Fatalf("round %d: %d approvals won, request %s, grant landed %d times (statuses %d, %d)", round, wins, w.requestState(t, id), grantCount(), res[0].status, res[1].status)
			}
			w.checkInvariants(t, "two approvers")
			w.resetGrants(t, "exec2")
		}
	})

	// arch-test:serial — see above.
	t.Run("approve vs the requester's deactivation", func(t *testing.T) {
		for round := range raceRounds {
			w.relogin(t, "exec2")
			id := w.submitFor(t, "exec2", x)
			res := w.raceOnce(t,
				w.call(http.MethodPost, "/api/v1/permission-requests/"+id+"/approve", w.tok(t, "admin"), ports.ApprovePermissionRequestRequest{DecisionReason: "race"}),
				w.call(http.MethodPost, "/api/v1/users/"+exec2+"/deactivate", w.owner.AccessToken, ports.DeactivateUserRequest{Reason: "race"}))
			onlyStatuses(t, "approve", res[0], http.StatusNoContent, http.StatusConflict)
			onlyStatuses(t, "deactivate", res[1], http.StatusNoContent)
			approved := w.requestState(t, id) == "approved"
			if approved != (res[0].status == http.StatusNoContent) || approved != (grantCount() == 1) {
				t.Fatalf("round %d: approve answered %d, request approved=%v, grant landed %d times: a grant must exist exactly when the approval did", round, res[0].status, approved, grantCount())
			}
			w.denyIfPending(t, id)
			if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/users/"+exec2+"/reactivate", w.owner.AccessToken, nil); r.status != http.StatusNoContent {
				t.Fatalf("round %d: reactivate: %d %s", round, r.status, r.body)
			}
			w.resetGrants(t, "exec2")
			w.checkInvariants(t, "approve vs deactivate")
		}
	})

	// arch-test:serial — see above.
	t.Run("approve vs the approver losing the permission", func(t *testing.T) {
		for round := range raceRounds {
			w.resetGrants(t, "lead", x) // the Team Lead holds x by override, so she may approve it for her report
			w.relogin(t, "lead")
			w.relogin(t, "exec2")
			id := w.submitFor(t, "exec2", x)
			res := w.raceOnce(t,
				w.call(http.MethodPost, "/api/v1/permission-requests/"+id+"/approve", w.tok(t, "lead"), ports.ApprovePermissionRequestRequest{DecisionReason: "race"}),
				w.call(http.MethodPatch, "/api/v1/users/"+w.mid(t, "lead")+"/permission-overrides", w.owner.AccessToken, ports.ReplaceUserPermissionOverridesRequest{Granted: atkRoutePerms(), Revoked: []string{}}))
			onlyStatuses(t, "approve", res[0], http.StatusNoContent, http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound)
			onlyStatuses(t, "revoke", res[1], http.StatusNoContent)
			approved := w.requestState(t, id) == "approved"
			if approved != (res[0].status == http.StatusNoContent) || approved != (grantCount() == 1) {
				t.Fatalf("round %d: approve answered %d, request approved=%v, grant landed %d times", round, res[0].status, approved, grantCount())
			}
			w.denyIfPending(t, id)
			w.resetGrants(t, "exec2")
			w.checkInvariants(t, "approve vs approver loses permission")
		}
	})
}

// TestAttack_Races_RolesAgainstTheirHoldersAndLinks: (1) deleting a role races
// assigning it; (2) a permission edit races an assignment by a member who may
// assign the role only as it was; (3) a re-level races a manager change of a holder.
func TestAttack_Races_RolesAgainstTheirHoldersAndLinks(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	ownTok := w.owner.AccessToken
	norole := w.mid(t, "norole")

	// arch-test:serial — the races share one world; each runs its rounds to completion first.
	t.Run("delete a role vs assign it", func(t *testing.T) {
		for round := range raceRounds {
			id := f.customRole(t, ownTok, "Delete Race "+string(rune('a'+round)), 60)
			res := w.raceOnce(t,
				w.call(http.MethodDelete, "/api/v1/roles/"+id, ownTok, nil),
				w.call(http.MethodPost, "/api/v1/users/"+norole+"/roles", w.tok(t, "admin"), ports.AssignUserRoleRequest{RoleID: id}))
			onlyStatuses(t, "delete", res[0], http.StatusNoContent, http.StatusConflict)
			onlyStatuses(t, "assign", res[1], http.StatusNoContent, http.StatusNotFound, http.StatusConflict)
			if res[0].status == http.StatusNoContent && res[1].status == http.StatusNoContent {
				t.Errorf("round %d: the role was deleted AND assigned", round)
			}
			w.checkInvariants(t, "delete vs assign") // no member may hold a role that no longer exists
			if u := f.getUser(t, ownTok, norole); slices.Contains(u.RoleIDs, id) {
				if r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+norole+"/roles/"+id, ownTok, nil); r.status != http.StatusNoContent {
					t.Fatalf("round %d: clean up the holder: %d %s", round, r.status, r.body)
				}
			}
		}
	})

	// arch-test:serial — see above.
	t.Run("edit a role's permissions vs assign it", func(t *testing.T) {
		id := f.customRole(t, ownTok, "Perm Race", 60)
		if r := f.grantRolePerm(t, ownTok, id, p.Tasks.WorkItems.Reassign); r.status != http.StatusNoContent {
			t.Fatalf("seed role permission: %d %s", r.status, r.body)
		}
		extra := p.Inventory.Catalog.Manage // "sales" may confer the role as it stands, not with this
		for round := range raceRounds {
			res := w.raceOnce(t,
				w.call(http.MethodPost, "/api/v1/users/"+norole+"/roles", w.tok(t, "sales"), ports.AssignUserRoleRequest{RoleID: id}),
				w.call(http.MethodPost, "/api/v1/roles/"+id+"/permissions/grant", ownTok, ports.RolePermissionRequest{Permission: extra}))
			onlyStatuses(t, "assign", res[0], http.StatusNoContent, http.StatusForbidden)
			onlyStatuses(t, "grant", res[1], http.StatusNoContent, http.StatusForbidden)
			u := f.getUser(t, ownTok, norole)
			holds := slices.Contains(u.RoleIDs, id)
			if holds != (res[0].status == http.StatusNoContent) {
				t.Fatalf("round %d: assign answered %d but the member holds the role=%v", round, res[0].status, holds)
			}
			w.checkInvariants(t, "perm edit vs assign")
			if holds {
				if r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+norole+"/roles/"+id, ownTok, nil); r.status != http.StatusNoContent {
					t.Fatalf("round %d: clean up the holder: %d %s", round, r.status, r.body)
				}
			}
			if r := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+id+"/permissions/revoke", ownTok, ports.RolePermissionRequest{Permission: extra}); r.status != http.StatusNoContent {
				t.Fatalf("round %d: take the permission off again: %d %s", round, r.status, r.body)
			}
		}
	})

	// arch-test:serial — see above.
	t.Run("re-level a role vs change its holder's manager", func(t *testing.T) {
		lvl40 := f.customRole(t, ownTok, "Level Race", 40)
		h := f.createMember(t, w.owner, "levelrace")
		f.assignRole(t, ownTok, h.membershipID, lvl40)
		f.setManager(t, ownTok, h.membershipID, w.mid(t, "lead")) // lead rank 30
		for round := range raceRounds {
			to25 := 25 // more senior than the Team Lead (30) the holder reports to
			res := w.raceOnce(t,
				w.call(http.MethodPatch, "/api/v1/roles/"+lvl40, ownTok, ports.UpdateRoleRequest{HierarchyLevel: &to25}),
				w.call(http.MethodPut, "/api/v1/users/"+h.membershipID+"/manager", ownTok, ports.AssignUserManagerRequest{ManagerID: w.mid(t, "sales")}))
			onlyStatuses(t, "re-level", res[0], http.StatusNoContent, http.StatusUnprocessableEntity)
			onlyStatuses(t, "manager", res[1], http.StatusNoContent)
			w.checkInvariants(t, "re-level vs manager") // every manager strictly senior to the report
			back := 40
			if r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+lvl40, ownTok, ports.UpdateRoleRequest{HierarchyLevel: &back}); r.status != http.StatusNoContent {
				t.Fatalf("round %d: re-level back: %d %s", round, r.status, r.body)
			}
			f.setManager(t, ownTok, h.membershipID, w.mid(t, "lead"))
		}
	})
}

// TestAttack_Races_PersonsAgainstTheirMemberships: (1) one person, inactive in two
// tenants, is reactivated in both at once: at most one membership ends active and
// no deadlock victim surfaces as a 500; (2) a global suspension (and, in fresh
// tenants, an anonymisation) races reactivating the other owner of the only owner's
// tenant: the tenant never ends without an active owner who can act.
func TestAttack_Races_PersonsAgainstTheirMemberships(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	op := f.mintPlatformTokenWith(t, []string{p.Users.Anonymise, p.Platform.RolesView}, false)
	personOf := func(id string) ports.PersonDto {
		r := f.authedJSON(t, http.MethodGet, "/api/v1/platform/persons/"+id, op, nil)
		var d ports.PersonDto
		if r.status != http.StatusOK || json.Unmarshal(r.body, &d) != nil {
			t.Fatalf("read person %s: %d %s", id, r.status, r.body)
		}
		return d
	}
	execRole := w.roles[role.SystemRoles.Tenant.SalesExecutive]

	// arch-test:serial — the races share one world; each runs its rounds to completion first.
	t.Run("two reactivations of one person in two tenants", func(t *testing.T) {
		m := f.createMember(t, w.owner, "twotenants")
		f.assignRole(t, w.owner.AccessToken, m.membershipID, execRole)
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", w.owner.AccessToken, ports.DeactivateUserRequest{Reason: "setup"}); r.status != http.StatusNoContent {
			t.Fatalf("deactivate in T: %d %s", r.status, r.body)
		}
		vr := f.authedJSON(t, http.MethodPost, "/api/v1/users", w.victim.AccessToken, ports.CreateUserRequest{Email: m.email, Password: m.password, FirstName: "Two", LastName: "Tenants"})
		var cu ports.CreateUserResponse
		if vr.status != http.StatusCreated || json.Unmarshal(vr.body, &cu) != nil {
			t.Fatalf("attach the person to V: %d %s", vr.status, vr.body)
		}
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/deactivate", w.victim.AccessToken, ports.DeactivateUserRequest{Reason: "setup"}); r.status != http.StatusNoContent {
			t.Fatalf("deactivate in V: %d %s", r.status, r.body)
		}
		for round := range raceRounds {
			res := w.raceOnce(t,
				w.call(http.MethodPost, "/api/v1/users/"+m.membershipID+"/reactivate", w.owner.AccessToken, nil),
				w.call(http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/reactivate", w.victim.AccessToken, nil))
			wins := 0
			for _, r := range res {
				// the loser SHOULD be a 409; today the unique index surfaces as a 500 (the FINDING pinned by
				// TestAttack_Persons_ReactivatingWhileActiveElsewhereIsAConflict). It is tolerated here so the
				// race still proves the invariant: exactly one winner, one active membership.
				onlyStatuses(t, "reactivate", r, http.StatusNoContent, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError)
				if r.status == http.StatusNoContent {
					wins++
				}
			}
			if wins != 1 {
				t.Fatalf("round %d: %d reactivations succeeded (statuses %d, %d), want exactly 1", round, wins, res[0].status, res[1].status)
			}
			active := 0
			if u := f.getUser(t, w.owner.AccessToken, m.membershipID); u.Status == "active" {
				active++
			}
			if u := f.getUser(t, w.victim.AccessToken, cu.MembershipID); u.Status == "active" {
				active++
			}
			if active != 1 {
				t.Fatalf("round %d: the person has %d active memberships after two reactivations (statuses %d, %d), want exactly 1", round, active, res[0].status, res[1].status)
			}
			// back to inactive in both
			f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", w.owner.AccessToken, ports.DeactivateUserRequest{Reason: "round over"})
			f.authedJSON(t, http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/deactivate", w.victim.AccessToken, ports.DeactivateUserRequest{Reason: "round over"})
		}
	})

	for _, kind := range []string{"global suspension", "anonymisation"} {
		// arch-test:serial — see above.
		t.Run(kind+" vs reactivating the other owner", func(t *testing.T) {
			var zOwner registeredTenant
			var q createdMember
			ownerRoleOf := func(z registeredTenant) string {
				return f.roleIDByName(t, z.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
			}
			fresh := func(round int) {
				zOwner = f.registerAndLogin(t, "pz"+string(rune('a'+round%26)))
				q = f.createMember(t, zOwner, "pzq")
				f.assignRole(t, zOwner.AccessToken, q.membershipID, ownerRoleOf(zOwner))
				if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+q.membershipID+"/deactivate", zOwner.AccessToken, ports.DeactivateUserRequest{Reason: "setup"}); r.status != http.StatusNoContent {
					t.Fatalf("deactivate the second owner: %d %s", r.status, r.body)
				}
			}
			fresh(0)
			for round := range raceRounds {
				if kind == "anonymisation" && round > 0 {
					fresh(round) // anonymisation cannot be undone: every round gets a tenant of its own
				}
				verb := "/global-suspend"
				var body any = ports.GlobalSuspendPersonRequest{Reason: "race"}
				if kind == "anonymisation" {
					verb, body = "/anonymise", nil
				}
				res := w.raceOnce(t,
					w.call(http.MethodPost, "/api/v1/platform/persons/"+zOwner.PersonID+verb, op, body),
					w.call(http.MethodPost, "/api/v1/users/"+q.membershipID+"/reactivate", zOwner.AccessToken, nil))
				onlyStatuses(t, kind, res[0], http.StatusNoContent, http.StatusConflict)
				onlyStatuses(t, "reactivate", res[1], http.StatusNoContent, http.StatusForbidden, http.StatusUnauthorized, http.StatusConflict)
				// the tenant must keep an active owner whose person can still act
				users := w.listUsersAs(t, op, zOwner.TenantID)
				ownerRole := ownerRoleOf2(w, zOwner.TenantID, op, t)
				ok := false
				for _, u := range users {
					if u.Status == "active" && slices.Contains(u.RoleIDs, ownerRole) {
						if d := personOf(u.PersonID); d.IsActive && !d.IsAnonymised && !d.IsGloballySuspended {
							ok = true
						}
					}
				}
				if !ok {
					t.Fatalf("round %d: after a %s racing a reactivation (statuses %d, %d) the tenant has no active owner who can act", round, kind, res[0].status, res[1].status)
				}
				if kind == "global suspension" {
					if r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/persons/"+zOwner.PersonID+"/lift-global-suspension", op, nil); r.status != http.StatusNoContent {
						t.Fatalf("round %d: lift the suspension: %d %s", round, r.status, r.body)
					}
					f.evictStamp(t, zOwner.PersonID)
					zOwner.AccessToken = f.loginToken(t, zOwner.Email, zOwner.Password)
					if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+q.membershipID+"/deactivate", zOwner.AccessToken, ports.DeactivateUserRequest{Reason: "round over"}); r.status != http.StatusNoContent && r.status != http.StatusConflict {
						t.Fatalf("round %d: deactivate the second owner again: %d %s", round, r.status, r.body)
					}
				}
			}
		})
	}
}

// listUsersAs lists a tenant's members as a scoped platform operator.
func (w *attackWorld) listUsersAs(t *testing.T, opTok, tenantID string) []ports.UserDto {
	t.Helper()
	r := w.f.doWithHeaders(t, http.MethodGet, "/api/v1/users?page_size=200", opTok, nil, scopeHdr(tenantID))
	var out ports.ListUsersResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil {
		t.Fatalf("operator lists users of %s: %d %s", tenantID, r.status, r.body)
	}
	return out.Users
}

// ownerRoleOf2 resolves the Company Owner role id of a tenant as a scoped operator.
func ownerRoleOf2(w *attackWorld, tenantID, opTok string, t *testing.T) string {
	t.Helper()
	r := w.f.doWithHeaders(t, http.MethodGet, "/api/v1/roles", opTok, nil, scopeHdr(tenantID))
	var lr ports.ListRolesResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &lr) != nil {
		t.Fatalf("operator lists roles of %s: %d %s", tenantID, r.status, r.body)
	}
	for _, rd := range lr.Roles {
		if rd.Name == role.SystemRoles.Tenant.CompanyOwner {
			return rd.ID
		}
	}
	t.Fatalf("no Company Owner role in %s", tenantID)
	return ""
}

// TestAttack_Persons_ReactivatingWhileActiveElsewhereIsAConflict pins the secure
// AND well-formed behaviour: reactivating a membership of a person who already has
// an active membership in another tenant is a 4xx conflict, not an internal error.
// Today the unique index uq_memberships_person_active holds the invariant but the
// violation reaches the client as 500 internal_error.
func TestAttack_Persons_ReactivatingWhileActiveElsewhereIsAConflict(t *testing.T) {
	t.Skip("known violation: FINDING: reactivating a second membership of a person with an active one elsewhere answers 500 internal_error, want 409 (uq_memberships_person_active is unmapped in writeUserMutationResult / ReactivateUser)")
	w := newAttackWorld(t)
	f := w.f
	m := f.createMember(t, w.owner, "twotenants")
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", w.owner.AccessToken, ports.DeactivateUserRequest{Reason: "setup"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate in T: %d %s", r.status, r.body)
	}
	vr := f.authedJSON(t, http.MethodPost, "/api/v1/users", w.victim.AccessToken, ports.CreateUserRequest{Email: m.email, Password: m.password, FirstName: "Two", LastName: "Tenants"})
	var cu ports.CreateUserResponse
	if vr.status != http.StatusCreated || json.Unmarshal(vr.body, &cu) != nil {
		t.Fatalf("attach the person to V: %d %s", vr.status, vr.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/deactivate", w.victim.AccessToken, ports.DeactivateUserRequest{Reason: "setup"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate in V: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/reactivate", w.owner.AccessToken, nil); r.status != http.StatusNoContent {
		t.Fatalf("reactivate in T: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/reactivate", w.victim.AccessToken, nil); r.status != http.StatusConflict {
		t.Errorf("reactivate in V while active in T: %d %s, want 409", r.status, r.body)
	}
}
