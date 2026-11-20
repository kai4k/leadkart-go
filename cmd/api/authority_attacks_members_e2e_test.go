//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goal 5 (and the membership half of goals 1, 2 and 3): act on a
// peer, a senior or oneself. Every persona is pointed at every member of the
// tenant it does not strictly outrank, through every membership command. The
// whole matrix must be refused AND leave the world exactly as it was.

package main

import (
	"net/http"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// atkTargets are the members a persona is pointed at. "gone" is deactivated.
var atkTargets = []string{"owner", "owner2", "admin", "office", "adminRO", "senior", "sales", "hr", "lead", "custom", "exec", "exec2", "norole", "gone"}

// atkActors are the personas that attack. The registered owner is not one: an
// owner may act on another owner, so there is nothing to refuse.
var atkActors = []string{"owner2", "admin", "office", "adminRO", "senior", "sales", "hr", "lead", "custom", "exec", "exec2", "norole"}

// targetRank is the rank a target was built with.
func (w *attackWorld) targetRank(key string) int {
	if key == "gone" {
		return 50
	}
	return w.rank(key)
}

func (w *attackWorld) targetID(t *testing.T, key string) string {
	t.Helper()
	if key == "gone" {
		return w.gone.membershipID
	}
	return w.mid(t, key)
}

// TestAttack_Members_NobodyActsOnAPeerASeniorOrThemselves is goal 5 across
// every persona and every membership command.
func TestAttack_Members_NobodyActsOnAPeerASeniorOrThemselves(t *testing.T) {
	w := newAttackWorld(t)
	execRole := w.roles[role.SystemRoles.Tenant.SalesExecutive]
	var rows []attack
	for _, actor := range atkActors {
		for _, target := range atkTargets {
			selfTarget := target == actor
			// An actor strictly outranking the target may act; those pairs are not attacks.
			if !selfTarget && w.rank(actor) < w.targetRank(target) {
				continue
			}
			// The owner role is not an owner pair for owner2: the registered owner is another owner.
			if actor == "owner2" && target != "owner2" {
				continue
			}
			id := w.targetID(t, target)
			status, code := http.StatusForbidden, "insufficient_rank"
			switch {
			case actor == "adminRO":
				code = "" // the route gate answers first: the read-bundle Administrator holds none of the write permissions
			case selfTarget:
				code = "self_modification_forbidden"
			}
			base := "/api/v1/users/" + id
			add := func(what, method, path string, body any) {
				rows = append(rows, attack{who: actor, what: what + " -> " + target, method: method, path: path, body: body, status: status, code: code})
			}
			add("assign role", http.MethodPost, base+"/roles", ports.AssignUserRoleRequest{RoleID: execRole})
			add("revoke role", http.MethodDelete, base+"/roles/"+execRole, nil)
			add("replace overrides", http.MethodPatch, base+"/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}})
			add("deactivate", http.MethodPost, base+"/deactivate", ports.DeactivateUserRequest{Reason: "attack"})
			add("reactivate", http.MethodPost, base+"/reactivate", nil)
			mgr := w.mid(t, actor) // self-appointment: the actor makes themselves the target's manager
			if selfTarget {
				mgr = w.mid(t, "owner") // appointing oneself manager of oneself is a different (422) rule
			}
			add("set manager", http.MethodPut, base+"/manager", ports.AssignUserManagerRequest{ManagerID: mgr})
			add("clear manager", http.MethodDelete, base+"/manager", nil)
			if !selfTarget { // editing your own profile is allowed
				add("edit profile", http.MethodPatch, base+"/profile", ports.UpdateUserProfileRequest{Designation: "pwned"})
			}
		}
	}
	w.assertGrouped(t, rows)
}

// TestAttack_Members_RoleConferralTables is goals 1 and 3 for role assignment:
// each persona is offered every role above or at its own level for a JUNIOR
// target it may otherwise act on (the colluder / a role-less member), and for
// itself nothing changes.
func TestAttack_Members_RoleConferralTables(t *testing.T) {
	w := newAttackWorld(t)
	rr := role.SystemRoles.Tenant
	// level of each seeded role, as seeded
	levels := map[string]int{rr.CompanyOwner: 0, rr.Administrator: 10, rr.OfficeAdministrator: 10, rr.SeniorManager: 15, rr.SalesManager: 20, rr.HrManager: 20, rr.TeamLead: 30, rr.SalesExecutive: 50, customRoleName: 40}
	var rows []attack
	for _, actor := range []string{"admin", "office", "senior", "sales", "hr", "lead", "custom"} {
		for roleName, lvl := range levels {
			if lvl > w.rank(actor) {
				continue // strictly below the actor: that is a legitimate assignment
			}
			// norole (rank 99) is junior to every actor: the target is fine, the ROLE is the attack.
			code := "insufficient_rank"
			rows = append(rows, attack{who: actor, what: "confer " + roleName + " on norole", method: http.MethodPost,
				path: "/api/v1/users/" + w.mid(t, "norole") + "/roles", body: ports.AssignUserRoleRequest{RoleID: w.roles[roleName]},
				status: http.StatusForbidden, code: code})
			rows = append(rows, attack{who: actor, what: "confer " + roleName + " on exec2", method: http.MethodPost,
				path: "/api/v1/users/" + w.mid(t, "exec2") + "/roles", body: ports.AssignUserRoleRequest{RoleID: w.roles[roleName]},
				status: http.StatusForbidden, code: code})
		}
	}
	w.assertGrouped(t, slices.Clone(rows))
}

// TestAttack_Members_ManagerLinksCannotLoopOrInvert is goal 5 for the reporting
// chain, with an actor who is entitled to act (the owner): the link itself must
// still be refused when it would close a loop, put a junior or a peer over a
// member, name an inactive or foreign member, or point at the member itself.
func TestAttack_Members_ManagerLinksCannotLoopOrInvert(t *testing.T) {
	w := newAttackWorld(t)
	// chain: exec -> lead -> sales -> senior -> admin ; exec2 -> lead ; hr -> senior
	put := func(what, member, manager string, status int, code string) attack {
		return attack{who: "owner", what: what, method: http.MethodPut, path: "/api/v1/users/" + member + "/manager",
			body: ports.AssignUserManagerRequest{ManagerID: manager}, status: status, code: code}
	}
	m := func(key string) string { return w.mid(t, key) }
	w.assertRows(t, []attack{
		put("close the loop: the top of the chain reports to the bottom", m("admin"), m("exec"), http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("close a loop one step up", m("senior"), m("lead"), http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("a junior over a senior", m("senior"), m("exec2"), http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("a peer over a peer (managers)", m("sales"), m("hr"), http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("a peer over a peer (executives)", m("exec"), m("exec2"), http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("an unroled member over an executive", m("exec"), m("norole"), http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("an inactive member as the manager", m("lead"), w.goneOwner.membershipID, http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("another tenant's member as the manager", m("lead"), w.vExec.membershipID, http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("a member that does not exist as the manager", m("lead"), "01a1f6e5-0000-7000-8000-000000000001", http.StatusUnprocessableEntity, "manager_not_eligible"),
		put("a member as their own manager", m("lead"), m("lead"), http.StatusUnprocessableEntity, "user_invalid"),
		// Administrator over the Senior Manager is the one valid direction; the tenant's owners have no manager
		put("an executive over an owner", m("owner2"), m("exec"), http.StatusUnprocessableEntity, "manager_not_eligible"),
	})
}
