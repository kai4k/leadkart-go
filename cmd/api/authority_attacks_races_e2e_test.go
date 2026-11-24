//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goal 9: break an invariant by racing. Every race runs
// raceRounds rounds of REAL concurrent HTTP against the real mux and database;
// after every round the tenant-wide invariants are checked, plus the invariant
// the race is about. A race never asserts WHO wins, only what must hold whoever
// does.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// raceRounds is the number of rounds each race runs per execution.
const raceRounds = 20

// reqFn is one racer: a request issued once the starting gun fires.
type reqFn func(ctx context.Context) (httpResp, error)

// call builds a racer.
func (w *attackWorld) call(method, path, tok string, body any) reqFn {
	return func(ctx context.Context) (httpResp, error) { return w.f.tryDo(ctx, method, path, tok, body, nil) }
}

// raceOnce fires every racer at the same instant and returns the responses in order.
func (w *attackWorld) raceOnce(t *testing.T, racers ...reqFn) []httpResp {
	t.Helper()
	res := make([]httpResp, len(racers))
	errs := make([]error, len(racers))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res[i], errs[i] = fn(t.Context())
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	return res
}

// relogin refreshes a persona's token (its stamp may have rotated).
func (w *attackWorld) relogin(t *testing.T, key string) {
	t.Helper()
	// the stamp cache may still hold the person's previous stamp: evict it as the cascade would
	if key == "owner" {
		w.f.evictStamp(t, w.owner.PersonID)
	} else {
		w.f.evictStamp(t, w.who[key].personID)
	}
	if key == "owner" {
		w.owner.AccessToken = w.f.loginToken(t, w.owner.Email, w.owner.Password)
		return
	}
	p := w.who[key]
	p.tok = w.f.loginToken(t, p.email, p.password)
}

// userByID finds a member in a list.
func userByID(users []ports.UserDto, id string) (ports.UserDto, bool) {
	for _, u := range users {
		if u.MembershipID == id {
			return u, true
		}
	}
	return ports.UserDto{}, false
}

// checkInvariants asserts the tenant-wide invariants as the (never raced) Office
// Administrator sees them: at least one active owner; no assignment to a role that
// no longer exists; no reporting cycle; every manager strictly senior to the report.
func (w *attackWorld) checkInvariants(t *testing.T, when string) {
	t.Helper()
	users := w.listUsers(t, w.tok(t, "office"))
	rr := w.f.authedJSON(t, http.MethodGet, "/api/v1/roles?status=all", w.tok(t, "office"), nil)
	var roles ports.ListRolesResponse
	if rr.status != http.StatusOK || json.Unmarshal(rr.body, &roles) != nil {
		t.Fatalf("%s: list roles: %d %s", when, rr.status, rr.body)
	}
	level := map[string]int{}
	inactive := map[string]bool{}
	for _, r := range roles.Roles {
		level[r.ID] = r.HierarchyLevel
		inactive[r.ID] = !r.IsActive
	}
	rank := func(u ports.UserDto) int {
		best := 99
		for _, id := range u.RoleIDs {
			if l, ok := level[id]; ok {
				best = min(best, l)
			}
		}
		return best
	}
	owners := 0
	for _, u := range users {
		for _, id := range u.RoleIDs {
			if _, ok := level[id]; !ok {
				t.Errorf("%s: member %s holds role %s, which no longer exists", when, u.MembershipID, id)
			}
			// ADR 0073: a role goes inactive only with no active holder, and an inactive role takes
			// no new holder, so an ACTIVE member never holds an inactive role through a race.
			if inactive[id] && u.Status == "active" {
				t.Errorf("%s: active member %s holds the inactive role %s", when, u.MembershipID, id)
			}
		}
		if u.Status == "active" && slices.Contains(u.RoleIDs, w.roles[role.SystemRoles.Tenant.CompanyOwner]) {
			owners++
		}
		if u.ReportsTo == "" {
			continue
		}
		mgr, ok := userByID(users, u.ReportsTo)
		if !ok {
			continue // an inactive manager is not listed
		}
		if rank(mgr) >= rank(u) {
			t.Errorf("%s: member %s (rank %d) reports to %s (rank %d): the manager is not strictly senior", when, u.MembershipID, rank(u), mgr.MembershipID, rank(mgr))
		}
		// a cycle: walk up at most len(users) steps
		cur, steps := u, 0
		for cur.ReportsTo != "" && steps <= len(users) {
			next, ok := userByID(users, cur.ReportsTo)
			if !ok {
				break
			}
			cur = next
			steps++
		}
		if steps > len(users) {
			t.Errorf("%s: the reporting chain of %s loops", when, u.MembershipID)
		}
	}
	if owners < 1 {
		t.Errorf("%s: the tenant has no active owner", when)
	}
}

// onlyStatuses fails when a response is not one of the statuses allowed.
func onlyStatuses(t *testing.T, what string, r httpResp, allowed ...int) {
	t.Helper()
	if !slices.Contains(allowed, r.status) {
		t.Errorf("%s: %d %s, want one of %v", what, r.status, r.body, allowed)
	}
}

// TestAttack_Races_OwnersRemovingEachOtherAcrossCommands: owner A deactivates owner
// B while B revokes A's Company Owner role (the crossed commands; the same-command
// races are in membership_concurrency_e2e_test.go). At most one removal wins, the
// tenant keeps an active owner, and the survivors repair the loser for the next round.
func TestAttack_Races_OwnersRemovingEachOtherAcrossCommands(t *testing.T) {
	w := newAttackWorld(t)
	ownerRole := w.roles[role.SystemRoles.Tenant.CompanyOwner]
	a, b := w.owner.MembershipID, w.mid(t, "owner2")
	for round := range raceRounds {
		aTok, bTok := w.owner.AccessToken, w.tok(t, "owner2")
		res := w.raceOnce(t,
			w.call(http.MethodPost, "/api/v1/users/"+b+"/deactivate", aTok, ports.DeactivateUserRequest{Reason: "owner race"}),
			w.call(http.MethodDelete, "/api/v1/users/"+a+"/roles/"+ownerRole, bTok, nil))
		for _, r := range res {
			onlyStatuses(t, "crossed owner removal", r, http.StatusNoContent, http.StatusForbidden, http.StatusConflict, http.StatusUnauthorized)
		}
		if res[0].status == http.StatusNoContent && res[1].status == http.StatusNoContent {
			t.Fatalf("round %d: both removals succeeded", round)
		}
		w.checkInvariants(t, "owners round")
		// repair: whoever is still a healthy owner puts the other back
		ua := w.f.getUser(t, w.tok(t, "office"), a) // the list is active-only; a deactivated owner is read by id
		ub := w.f.getUser(t, w.tok(t, "office"), b)
		healthy := func(u ports.UserDto) bool { return u.Status == "active" && slices.Contains(u.RoleIDs, ownerRole) }
		switch {
		case healthy(ua) && healthy(ub):
		case healthy(ua):
			w.relogin(t, "owner")
			w.repairOwner(t, w.owner.AccessToken, ub, ownerRole)
			w.relogin(t, "owner2")
		case healthy(ub):
			w.relogin(t, "owner2")
			w.repairOwner(t, w.tok(t, "owner2"), ua, ownerRole)
			w.relogin(t, "owner")
		default:
			t.Fatalf("round %d: no healthy owner left", round)
		}
	}
}

// repairOwner puts an owner back to active with the Company Owner role.
func (w *attackWorld) repairOwner(t *testing.T, healthyTok string, u ports.UserDto, ownerRole string) {
	t.Helper()
	if u.Status != "active" {
		if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/users/"+u.MembershipID+"/reactivate", healthyTok, nil); r.status != http.StatusNoContent {
			t.Fatalf("repair: reactivate: %d %s", r.status, r.body)
		}
	}
	if !slices.Contains(u.RoleIDs, ownerRole) {
		w.f.assignRole(t, healthyTok, u.MembershipID, ownerRole)
	}
}

// TestAttack_Races_ProfileEditCannotUndoAnAuthorityCommand: an Administrator edits
// a member's profile while the owner changes that member's roles, overrides,
// status or manager. The profile edit writes whole-aggregate; it must not write a
// stale role set, status, override list or manager link back over a committed command.
func TestAttack_Races_ProfileEditCannotUndoAnAuthorityCommand(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	ownTok, admTok := w.owner.AccessToken, w.tok(t, "admin")
	tgt := f.createMember(t, w.owner, "profilerace")
	execRole, leadRole := w.roles[role.SystemRoles.Tenant.SalesExecutive], w.roles[role.SystemRoles.Tenant.TeamLead]
	f.assignRole(t, ownTok, tgt.membershipID, execRole)
	base := "/api/v1/users/" + tgt.membershipID
	type command struct {
		name    string
		do      reqFn
		undo    func()
		applied func(u ports.UserDto, ov string) bool
	}
	cmds := []command{
		{"deactivate", w.call(http.MethodPost, base+"/deactivate", ownTok, ports.DeactivateUserRequest{Reason: "profile race"}),
			func() { f.authedJSON(t, http.MethodPost, base+"/reactivate", ownTok, nil) },
			func(u ports.UserDto, _ string) bool { return u.Status == "inactive" }},
		{"revoke the role", w.call(http.MethodDelete, base+"/roles/"+execRole, ownTok, nil),
			func() { f.assignRole(t, ownTok, tgt.membershipID, execRole) },
			func(u ports.UserDto, _ string) bool { return !slices.Contains(u.RoleIDs, execRole) }},
		{"assign another role", w.call(http.MethodPost, base+"/roles", ownTok, ports.AssignUserRoleRequest{RoleID: leadRole}),
			func() { f.authedJSON(t, http.MethodDelete, base+"/roles/"+leadRole, ownTok, nil) },
			func(u ports.UserDto, _ string) bool { return slices.Contains(u.RoleIDs, leadRole) }},
		{"replace overrides", w.call(http.MethodPatch, base+"/permission-overrides", ownTok, ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Crm.Leads.Read}, Revoked: []string{}}),
			func() { f.grantOverrides(t, ownTok, tgt.membershipID, []string{}, nil) },
			func(_ ports.UserDto, ov string) bool { return strings.Contains(ov, p.Crm.Leads.Read) }},
		{"set the manager", w.call(http.MethodPut, base+"/manager", ownTok, ports.AssignUserManagerRequest{ManagerID: w.mid(t, "sales")}),
			func() { f.authedJSON(t, http.MethodDelete, base+"/manager", ownTok, nil) },
			func(u ports.UserDto, _ string) bool { return u.ReportsTo == w.mid(t, "sales") }},
	}
	for _, c := range cmds {
		// arch-test:serial — the commands share one world and one target; each runs its rounds to completion first.
		t.Run(c.name, func(t *testing.T) {
			for round := range raceRounds {
				edit := w.call(http.MethodPatch, base+"/profile", admTok, ports.UpdateUserProfileRequest{Designation: "round " + string(rune('a'+round)), Department: "race"})
				res := w.raceOnce(t, c.do, edit)
				onlyStatuses(t, c.name, res[0], http.StatusNoContent)
				onlyStatuses(t, "profile edit", res[1], http.StatusNoContent, http.StatusConflict, http.StatusForbidden)
				got := f.getUser(t, ownTok, tgt.membershipID)
				if res[0].status == http.StatusNoContent && !c.applied(got, w.overridesOf(t, w.owner.TenantID, tgt.membershipID)) {
					t.Fatalf("round %d: %s answered 204 but the profile edit wrote the old state back: %+v", round, c.name, got)
				}
				w.checkInvariants(t, c.name)
				c.undo()
				if u := f.getUser(t, ownTok, tgt.membershipID); u.Status != "active" || !slices.Contains(u.RoleIDs, execRole) || len(u.RoleIDs) != 1 || u.ReportsTo != "" {
					t.Fatalf("round %d: the undo of %s left %+v", round, c.name, u)
				}
			}
		})
	}
}
