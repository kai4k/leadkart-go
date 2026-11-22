//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).
//
// arch-test:no-synctest — the races cross the real pgx pool and Postgres row
// locks; a virtual clock cannot order them.

// ADR 0069 / hierarchy round 4, I1: every writer of a membership serialises on
// the membership row. A profile transaction that loaded a member before an
// authority change committed must not write that stale snapshot back over the
// change. Each test interleaves one slow profile transaction (holding a stale
// load) with a real authority command over HTTP, both really committing.

package main

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// staleProfileWindow is how long the slow profile transaction waits, holding
// its loaded snapshot, for the competing command to finish. Without the row
// lock the command finishes inside the window and the profile transaction then
// writes its stale snapshot; with the row lock the command is blocked, the
// window expires, the profile transaction commits first and the command then
// applies to the committed state.
const staleProfileWindow = 600 * time.Millisecond

const staleProfileMessage = "profile edited under a stale load"

// slowProfileUpdate runs a profile update through the membership repository
// whose updateFn signals `loaded`, then waits (up to staleProfileWindow) for
// `other` to close before mutating. It returns when the transaction committed.
func slowProfileUpdate(t *testing.T, f e2eFixture, tenantID, membershipID string, loaded chan<- struct{}, other <-chan struct{}) {
	t.Helper()
	repo := adapters.NewMembershipRepository(f.Pool, pg.NewTransactor(f.Pool))
	err := repo.UpdateByID(t.Context(), tenant.ID(tenantID), membership.ID(membershipID), func(m *membership.Membership) (bool, error) {
		close(loaded)
		select {
		case <-other:
		case <-time.After(staleProfileWindow):
		}
		if err := m.UpdateProfile("Regional Head", "Sales", staleProfileMessage, time.Now()); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("slow profile update: %v", err)
	}
}

func TestE2E_MembershipRowLock_ProfileEditCannotUndoRoleRevoke(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "rowlock-role")
	exec := f.createMember(t, owner, "exec")
	execRole := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	f.assignRole(t, owner.AccessToken, exec.membershipID, execRole)

	loaded := make(chan struct{})
	revoked := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { slowProfileUpdate(t, f, owner.TenantID, exec.membershipID, loaded, revoked) })
	<-loaded
	r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+exec.membershipID+"/roles/"+execRole, owner.AccessToken, nil)
	close(revoked)
	wg.Wait()
	if r.status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	u := f.getUser(t, owner.AccessToken, exec.membershipID)
	if slices.Contains(u.RoleIDs, execRole) {
		t.Fatalf("the member still holds the revoked role %v: a profile edit undid an authority decision that was answered 204", u.RoleIDs)
	}
	if u.StatusMessage != staleProfileMessage {
		t.Errorf("the profile edit itself was lost: status message %q", u.StatusMessage)
	}
}

func TestE2E_MembershipRowLock_ProfileEditCannotUndoDeactivate(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "rowlock-deact")
	exec := f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, exec.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))

	loaded := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { slowProfileUpdate(t, f, owner.TenantID, exec.membershipID, loaded, done) })
	<-loaded
	r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+exec.membershipID+"/deactivate", owner.AccessToken,
		ports.DeactivateUserRequest{Reason: "row lock race"})
	close(done)
	wg.Wait()
	if r.status != http.StatusNoContent {
		t.Fatalf("deactivate: %d %s", r.status, r.body)
	}
	if u := f.getUser(t, owner.AccessToken, exec.membershipID); u.Status != "inactive" {
		t.Fatalf("status = %q after a 204 deactivate: a profile edit reactivated the member", u.Status)
	}
}

// TestE2E_MembershipRowLock_ProfileEditCannotLeaveTenantWithoutOwner is the
// no-malice interleaving: HR edits owner O1's profile (loaded while O1 was
// inactive); meanwhile O2 reactivates O1 and O1 deactivates O2. The stale
// profile write must not put O1 back to inactive: the tenant keeps an owner.
func TestE2E_MembershipRowLock_ProfileEditCannotLeaveTenantWithoutOwner(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	o2 := f.registerAndLogin(t, "rowlock-owner")
	ownerRole := f.roleIDByName(t, o2.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
	o1 := f.addSecondOwner(t, o2, "owner-one")
	adminM := f.createMember(t, o2, "observer")
	f.assignRole(t, o2.AccessToken, adminM.membershipID, f.roleIDByName(t, o2.AccessToken, role.SystemRoles.Tenant.Administrator))
	observer := f.loginToken(t, adminM.email, adminM.password)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+o1.membershipID+"/deactivate", o2.AccessToken,
		ports.DeactivateUserRequest{Reason: "setup: O1 inactive"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate O1: %d %s", r.status, r.body)
	}

	loaded := make(chan struct{})
	sequenceDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { slowProfileUpdate(t, f, o2.TenantID, o1.membershipID, loaded, sequenceDone) })
	<-loaded
	// O2 reactivates O1, then O1 (now active, fresh login) deactivates O2.
	reactivate := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+o1.membershipID+"/reactivate", o2.AccessToken, nil)
	var deactivate httpResp
	if reactivate.status == http.StatusNoContent {
		o1Tok := f.loginToken(t, o1.email, o1.password)
		deactivate = f.authedJSON(t, http.MethodPost, "/api/v1/users/"+o2.MembershipID+"/deactivate", o1Tok,
			ports.DeactivateUserRequest{Reason: "owner swap"})
	}
	close(sequenceDone)
	wg.Wait()
	if reactivate.status != http.StatusNoContent {
		t.Fatalf("reactivate O1: %d %s", reactivate.status, reactivate.body)
	}
	if deactivate.status != http.StatusNoContent {
		t.Fatalf("O1 deactivates O2: %d %s", deactivate.status, deactivate.body)
	}
	if got := f.activeOwners(t, observer, ownerRole); got < 1 {
		t.Fatalf("the tenant has no active owner left (%d): a stale profile write overwrote the reactivation", got)
	}
}

// TestE2E_Overrides_DuplicateRevokedIsOneAndOverlapIsAFieldError: the same
// revoked name twice is accepted (stored once, no 500); a name in both lists
// answers 422 validation_failed naming the field, and changes nothing.
func TestE2E_Overrides_DuplicateRevokedIsOneAndOverlapIsAFieldError(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "overrides-dup")
	exec := f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, exec.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	const name = "crm.leads.read"
	path := "/api/v1/users/" + exec.membershipID + "/permission-overrides"

	r := f.authedJSON(t, http.MethodPatch, path, owner.AccessToken,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{name, name}})
	if r.status != http.StatusNoContent {
		t.Fatalf("duplicate revoked name: %d %s, want 204", r.status, r.body)
	}
	r = f.authedJSON(t, http.MethodPatch, path, owner.AccessToken,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{name}, Revoked: []string{name}})
	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("name in both lists: %d %s, want 422", r.status, r.body)
	}
	er := decodeError(t, r.body)
	if er.Error != "validation_failed" || len(er.Errors["revoked"]) != 1 || er.Errors["revoked"][0] != "also_granted" {
		t.Fatalf("422 body = %s, want validation_failed with errors.revoked = [also_granted]", r.body)
	}
}

// TestE2E_PersonRowLock_StampRotationCannotRevertAPasswordChange: the person
// aggregate is persisted whole (UpdatePerson rewrites every column), so a
// writer holding a stale load — here a stamp rotation — must not write back the
// old password hash over a password change that committed meanwhile.
func TestE2E_PersonRowLock_StampRotationCannotRevertAPasswordChange(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "personlock")
	persons := adapters.NewPersonRepository(f.Pool, pg.NewTransactor(f.Pool))

	loaded := make(chan struct{})
	changed := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		err := persons.UpdateByID(t.Context(), person.ID(owner.PersonID), func(p *person.Person) (bool, error) {
			close(loaded)
			select {
			case <-changed:
			case <-time.After(staleProfileWindow):
			}
			return true, p.RotateSecurityStamp("role_revoked", time.Now())
		})
		if err != nil {
			t.Errorf("slow stamp rotation: %v", err)
		}
	})
	<-loaded
	newPassword := owner.Password + "-v2"
	r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", owner.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: owner.Password, NewPassword: newPassword})
	close(changed)
	wg.Wait()
	if r.status != http.StatusNoContent {
		t.Fatalf("change-password: %d %s", r.status, r.body)
	}
	if lr := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: owner.Email, Password: newPassword, DeviceLabel: "e2e"}); lr.status != http.StatusOK {
		t.Fatalf("login with the NEW password: %d %s — a stale person write reverted the password change", lr.status, lr.body)
	}
}

// TestE2E_PersonChangeGuard_ReactivationCannotCommitBetweenTheCheckAndTheChange:
// the guard lists a person's memberships and, finding no active one, used to
// apply the person-level change with no lock at all, so a reactivation of that
// person in another tenant could commit between the check and the change and
// leave that tenant with an owner nobody guarded. The guard now holds the person
// row for the change (reactivation takes the same row first), so the membership
// set the guard checked is the set the change sees.
func TestE2E_PersonChangeGuard_ReactivationCannotCommitBetweenTheCheckAndTheChange(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	q := f.registerAndLogin(t, "guard-race")
	p := f.addSecondOwner(t, q, "owner-p")
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+p.membershipID+"/deactivate", q.AccessToken,
		ports.DeactivateUserRequest{Reason: "setup: P inactive"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate P: %d %s", r.status, r.body)
	}
	tx := pg.NewTransactor(f.Pool)
	memberships := adapters.NewMembershipRepository(f.Pool, tx)
	guard := command.NewPersonChangeGuard(tx, adapters.NewAuthorityLockPG(), memberships, adapters.NewRoleRepository(f.Pool, tx),
		adapters.NewPersonRepository(f.Pool, tx), time.Now)

	inChange := make(chan struct{})
	reactivated := make(chan struct{})
	var activeDuringChange bool
	var wg sync.WaitGroup
	wg.Go(func() {
		err := guard.Run(t.Context(), person.ID(p.personID), func(_ context.Context) error {
			close(inChange)
			select {
			case <-reactivated:
			case <-time.After(staleProfileWindow):
			}
			// Read what is COMMITTED right now, from outside the guard's transaction.
			all, err := memberships.ListAllForPerson(t.Context(), person.ID(p.personID))
			if err != nil {
				return err
			}
			for _, m := range all {
				activeDuringChange = activeDuringChange || m.Status() == membership.StatusActive
			}
			return nil
		})
		if err != nil {
			t.Errorf("guard.Run: %v", err)
		}
	})
	<-inChange
	r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+p.membershipID+"/reactivate", q.AccessToken, nil)
	close(reactivated)
	wg.Wait()
	if r.status != http.StatusNoContent {
		t.Fatalf("reactivate P: %d %s, want 204 (it may only wait for the guarded change, not fail)", r.status, r.body)
	}
	if activeDuringChange {
		t.Fatal("a reactivation committed while the guard was applying a person-level change it had checked against a membership set without it")
	}
}
