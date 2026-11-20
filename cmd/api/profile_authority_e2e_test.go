//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Hierarchy round 4: editing ANOTHER member's profile is an authority decision
// (the caller must strictly outrank them). identity.users.update alone used to
// let an HR Manager rewrite an owner's profile.

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_ProfileEdit_OtherMembersAreRankGated(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "profile-rank")
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }
	hr := f.createMember(t, owner, "hr")
	f.assignRole(t, owner.AccessToken, hr.membershipID, roleID(role.SystemRoles.Tenant.HrManager))
	peer := f.createMember(t, owner, "peer")
	f.assignRole(t, owner.AccessToken, peer.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	exec := f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, exec.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	hrTok := f.loginToken(t, hr.email, hr.password)
	execTok := f.loginToken(t, exec.email, exec.password)
	body := ports.UpdateUserProfileRequest{Designation: "Edited", Department: "Edited", StatusMessage: "edited by test"}
	patch := func(tok string, m string) httpResp {
		return f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+m+"/profile", tok, body)
	}

	f.wantForbidden(t, "HR Manager edits the owner's profile", patch(hrTok, owner.MembershipID), http.StatusForbidden, "insufficient_rank")
	f.wantForbidden(t, "HR Manager edits a peer manager's profile", patch(hrTok, peer.membershipID), http.StatusForbidden, "insufficient_rank")
	if u := f.getUser(t, owner.AccessToken, owner.MembershipID); u.Designation == "Edited" {
		t.Error("the refused edit changed the owner's profile")
	}
	if r := patch(hrTok, exec.membershipID); r.status != http.StatusNoContent {
		t.Errorf("HR Manager edits an executive's profile: %d %s, want 204", r.status, r.body)
	}
	if r := patch(execTok, exec.membershipID); r.status != http.StatusNoContent {
		t.Errorf("an executive edits their own profile: %d %s, want 204 (self-edit unchanged)", r.status, r.body)
	}
	// The owner edits anyone's profile (owners may act on owners).
	if r := patch(owner.AccessToken, peer.membershipID); r.status != http.StatusNoContent {
		t.Errorf("the owner edits a manager's profile: %d %s, want 204", r.status, r.body)
	}
}
