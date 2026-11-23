//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// ADR 0069 H6: the last active owner of a tenant cannot be removed, however
// the removal is attempted and however many removals race. The per-tenant
// authority lock makes the check and the write one step; the person-level
// routes guard every tenant the person actively belongs to.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// addSecondOwner makes a new member of v's tenant a Company Owner and returns it.
func (f e2eFixture) addSecondOwner(t *testing.T, v registeredTenant, prefix string) createdMember {
	t.Helper()
	m := f.createMember(t, v, prefix)
	f.assignRole(t, v.AccessToken, m.membershipID, f.roleIDByName(t, v.AccessToken, role.SystemRoles.Tenant.CompanyOwner))
	return m
}

// activeOwners counts the tenant's active Company Owners as the owner (a
// third observer that is never part of a race) sees them.
func (f e2eFixture) activeOwners(t *testing.T, observer, ownerRole string) int {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/users?page_size=200", observer, nil)
	if r.status != http.StatusOK {
		t.Fatalf("list users: %d %s", r.status, r.body)
	}
	var out ports.ListUsersResponse
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	n := 0
	for _, u := range out.Users {
		if u.Status == "active" && slices.Contains(u.RoleIDs, ownerRole) {
			n++
		}
	}
	return n
}

// TestE2E_AuthorityLock_OwnersRemovingEachOtherLeaveOne: two owners remove each
// other at the same moment (deactivate, then revoke the Company Owner role),
// many times. Exactly one removal wins and the tenant always keeps an owner.
func TestE2E_AuthorityLock_OwnersRemovingEachOtherLeaveOne(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	first := f.registerAndLogin(t, "race")
	second := f.addSecondOwner(t, first, "owner-b")
	ownerRole := f.roleIDByName(t, first.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
	adminM := f.createMember(t, first, "observer")
	f.assignRole(t, first.AccessToken, adminM.membershipID, f.roleIDByName(t, first.AccessToken, role.SystemRoles.Tenant.Administrator))
	observer := f.loginToken(t, adminM.email, adminM.password)
	secondTok := f.loginToken(t, second.email, second.password)

	const rounds = 12
	type result struct{ status int }
	race := func(t *testing.T, name string, fire func(tok, targetID string) httpResp, restore func(winnerTok, loserID string)) {
		for i := range rounds {
			var wg sync.WaitGroup
			var res [2]result
			start := make(chan struct{})
			actors := [2]struct{ tok, self, other string }{
				{first.AccessToken, first.MembershipID, second.membershipID},
				{secondTok, second.membershipID, first.MembershipID},
			}
			for k := range actors {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					res[k] = result{fire(actors[k].tok, actors[k].other).status}
				}()
			}
			close(start)
			wg.Wait()
			wins := 0
			for _, r := range res {
				if r.status == http.StatusNoContent {
					wins++
				}
			}
			if wins > 1 {
				t.Fatalf("%s round %d: both removals succeeded (%d, %d): the tenant has no owner left", name, i, res[0].status, res[1].status)
			}
			if got := f.activeOwners(t, observer, ownerRole); got < 1 {
				t.Fatalf("%s round %d: %d active owners left (statuses %d, %d)", name, i, got, res[0].status, res[1].status)
			}
			// Whoever's request returned 204 removed the OTHER owner, so that
			// requester is the survivor and puts the loser back for the next round.
			switch {
			case res[0].status == http.StatusNoContent:
				restore(actors[0].tok, actors[1].self)
			case res[1].status == http.StatusNoContent:
				restore(actors[1].tok, actors[0].self)
			}
		}
	}

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("deactivate", func(t *testing.T) {
		race(t, "deactivate",
			func(tok, targetID string) httpResp {
				return f.authedJSON(t, http.MethodPost, "/api/v1/users/"+targetID+"/deactivate", tok, ports.DeactivateUserRequest{Reason: "owner race"})
			},
			func(winnerTok, loserID string) {
				if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+loserID+"/reactivate", winnerTok, nil); r.status != http.StatusNoContent {
					t.Fatalf("restore: reactivate %s: %d %s", loserID, r.status, r.body)
				}
			})
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("revoke the Company Owner role", func(t *testing.T) {
		race(t, "revoke",
			func(tok, targetID string) httpResp {
				return f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+targetID+"/roles/"+ownerRole, tok, nil)
			},
			func(winnerTok, loserID string) {
				f.assignRole(t, winnerTok, loserID, ownerRole)
			})
	})
}

// TestE2E_PersonLevel_LastOwnerCannotBeSuspended: the operator
// routes that take a person out of every tenant at once refuse (409
// last_owner_protected, nothing changed) when the person is a tenant's only
// active owner, and work once another active owner exists.
func TestE2E_PersonLevel_LastOwnerCannotBeSuspended(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	op := f.mintPlatformTokenWith(t, nil, false)
	v := f.registerAndLogin(t, "plast")

	// arch-test:serial — subtests share one fixture and one tenant.
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("global suspend", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/persons/"+v.PersonID+"/global-suspend", op, ports.GlobalSuspendPersonRequest{Reason: "owner guard test"})
		f.wantForbidden(t, "suspend the only owner", r, http.StatusConflict, "last_owner_protected")
		if g := f.authedJSON(t, http.MethodGet, "/api/v1/platform/persons/"+v.PersonID, op, nil); g.status != http.StatusOK || bytesContains(g.body, `"is_globally_suspended":true`) {
			t.Errorf("person after a refused suspend: %d %s", g.status, g.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a second active owner lifts the guard", func(t *testing.T) {
		f.addSecondOwner(t, v, "owner-b")
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/persons/"+v.PersonID+"/global-suspend", op, ports.GlobalSuspendPersonRequest{Reason: "owner guard test"}); r.status != http.StatusNoContent {
			t.Fatalf("suspend with a second owner: %d %s", r.status, r.body)
		}
	})
}

func bytesContains(b []byte, sub string) bool { return strings.Contains(string(b), sub) }
