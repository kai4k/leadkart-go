//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().
//
// arch-test:no-synctest — the expiry is decided on the injected clock (wall time
// plus an offset the test advances by hand); the requests cross HTTP and Postgres.

// End-to-end tests for the precondition of PATCH .../permission-overrides
// (identity fix round 2, I1): a save carries the overrides_version of the read it
// was made from and is refused, with nothing written, when the member's live
// overrides are no longer that state.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// wallClock is wall time plus an offset the test advances: a time-bound grant is
// minted against real time (the permission resolver reads the wall clock) and the
// test then moves the commands and the reads past its expiry without sleeping.
type wallClock struct{ offset atomic.Int64 }

func (c *wallClock) now() time.Time          { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *wallClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// storedGrants is the member's STORED granted overrides (expired ones included),
// name to expiry, read through the repository.
func (f e2eFixture) storedGrants(t *testing.T, tenantID, membershipID string) map[string]time.Time {
	t.Helper()
	m, err := adapters.NewMembershipRepository(f.Pool, pg.NewTransactor(f.Pool)).GetByID(t.Context(), tenant.ID(tenantID), membership.ID(membershipID))
	if err != nil {
		t.Fatalf("read membership %s: %v", membershipID, err)
	}
	out := map[string]time.Time{}
	for _, g := range m.GrantedPermissions() {
		out[g.Permission.Name()] = g.ExpiresAt
	}
	return out
}

func grantedNames(u ports.UserDto) []string {
	out := []string{}
	for _, g := range u.PermissionOverrides.Granted {
		out = append(out, g.Name)
	}
	return out
}

func expiryOf(u ports.UserDto, name string) (time.Time, bool) {
	for _, g := range u.PermissionOverrides.Granted {
		if g.Name == name {
			return g.ExpiresAt, true
		}
	}
	return time.Time{}, false
}

// The two ways the review found a form older than the stored overrides silently
// changing access, each end to end, plus the saves that must still work.
//
//   - B, the approval erased: the admin opens the form, a permission request is
//     approved, the admin saves. The save answers 409 and the approved grant stays.
//   - A, the expiry dropped: a time-bound grant is live when the form opens and has
//     expired when it is saved. The form re-sends the name; the save answers 409 and
//     the grant is NOT stored permanent.
func TestE2E_Overrides_AStaleFormCannotChangeAccess(t *testing.T) {
	clock := &wallClock{}
	f := newE2EFixtureAt(t, clock.now)
	owner := f.registerAndLogin(t, "ovver")
	p := permission.IdentityPermissions
	timedPerm, otherPerm := p.Inventory.Catalog.Manage, p.Tasks.WorkItems.Read
	exec := f.createMember(t, owner, "exec")
	own := owner.AccessToken
	f.assignRole(t, own, exec.membershipID, f.roleIDByName(t, own, role.SystemRoles.Tenant.SalesExecutive))
	path := "/api/v1/users/" + exec.membershipID + "/permission-overrides"
	save := func(tok string, granted []string, version string) httpResp {
		t.Helper()
		return f.authedJSON(t, http.MethodPatch, path, tok, ports.ReplaceUserPermissionOverridesRequest{Granted: granted, Revoked: []string{}, OverridesVersion: version})
	}
	mustConflict := func(what string, r httpResp) {
		t.Helper()
		if r.status != http.StatusConflict || decodeError(t, r.body).Error != ports.ErrCodeOverridesChanged {
			t.Fatalf("%s: %d %s, want 409 %s", what, r.status, r.body, ports.ErrCodeOverridesChanged)
		}
	}

	// ----- B: an approval lands after the form opened ------------------------
	opened := f.getUser(t, own, exec.membershipID)
	if opened.PermissionOverrides == nil || opened.OverridesVersion == "" || len(opened.PermissionOverrides.Granted) != 0 {
		t.Fatalf("the form opens on %+v version %q, want empty overrides and a version", opened.PermissionOverrides, opened.OverridesVersion)
	}
	sub, reqID := f.requestElevationFor(t, f.loginToken(t, exec.email, exec.password), timedPerm, 1)
	if sub.status != http.StatusCreated {
		t.Fatalf("submit: %d %s", sub.status, sub.body)
	}
	if r := f.approveRequest(t, own, reqID); r.status != http.StatusNoContent {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}
	mustConflict("B: a save from the form opened before the approval", save(own, []string{otherPerm}, opened.OverridesVersion))
	afterB := f.getUser(t, own, exec.membershipID)
	expires, ok := expiryOf(afterB, timedPerm)
	if !ok || expires.IsZero() || len(afterB.PermissionOverrides.Granted) != 1 {
		t.Fatalf("B: overrides after the refused save = %+v, want only the approved %s with its expiry (the save must not erase it)", afterB.PermissionOverrides.Granted, timedPerm)
	}
	if afterB.OverridesVersion == opened.OverridesVersion {
		t.Fatal("B: the version did not change when the approval landed")
	}

	// a save from the fresh read works, and the re-sent time-bound grant keeps its expiry
	if r := save(own, append(grantedNames(afterB), otherPerm), afterB.OverridesVersion); r.status != http.StatusNoContent {
		t.Fatalf("a save with the version of a fresh read: %d %s, want 204", r.status, r.body)
	}
	live := f.getUser(t, own, exec.membershipID)
	if got, _ := expiryOf(live, timedPerm); !got.Equal(expires) {
		t.Fatalf("the re-sent time-bound grant expires %v after the save, want its original %v", got, expires)
	}
	if got, ok := expiryOf(live, otherPerm); !ok || !got.IsZero() {
		t.Fatalf("the new grant %s is %v / present=%v, want a permanent grant", otherPerm, got, ok)
	}

	// a body with no version is refused before anything is decided
	r := f.authedJSON(t, http.MethodPatch, path, own, map[string]any{"granted": grantedNames(live), "revoked": []string{}})
	if e := decodeError(t, r.body); r.status != http.StatusBadRequest || e.Error != ports.ErrCodeInvalidBody || !slices.Equal(e.Errors["overrides_version"], []string{"required"}) {
		t.Fatalf("a save without overrides_version: %d %s, want 400 invalid_body with errors.overrides_version = [required]", r.status, r.body)
	}

	// ----- A: the time-bound grant expires after the form opened --------------
	// `live` is the form: it lists the time-bound grant, which is live now.
	clock.advance(25 * time.Hour) // the one-day grant has expired
	own = f.loginToken(t, owner.Email, owner.Password)
	mustConflict("A: a save re-sending a grant that expired after the form opened", save(own, grantedNames(live), live.OverridesVersion))
	stored := f.storedGrants(t, owner.TenantID, exec.membershipID)
	if got, ok := stored[timedPerm]; !ok || got.IsZero() || !got.Equal(expires) {
		t.Fatalf("A: the stored grant of %s is %v (present=%v), want its original expiry %v: a stale form must never make it permanent", timedPerm, got, ok, expires)
	}
	afterA := f.getUser(t, own, exec.membershipID)
	if _, listed := expiryOf(afterA, timedPerm); listed || afterA.OverridesVersion == live.OverridesVersion {
		t.Fatalf("A: the read after the expiry lists %+v with version %q (the form's was %q), want the expired grant gone and a new version", afterA.PermissionOverrides.Granted, afterA.OverridesVersion, live.OverridesVersion)
	}
	// the admin looks again and saves what is there now
	if r := save(own, grantedNames(afterA), afterA.OverridesVersion); r.status != http.StatusNoContent {
		t.Fatalf("A: a save from the read after the expiry: %d %s, want 204", r.status, r.body)
	}
}

// The version is a precondition, not a credential: a caller who may not act on
// the member is refused by rank whatever version they send, and the overrides
// version is on the read only for callers shown the overrides.
func TestE2E_Overrides_TheVersionIsShownAndCheckedOnlyForThoseWhoMayChangeThem(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "ovrank")
	p := permission.IdentityPermissions
	own := owner.AccessToken
	execRole := f.roleIDByName(t, own, role.SystemRoles.Tenant.SalesExecutive)
	exec, peer := f.createMember(t, owner, "exec"), f.createMember(t, owner, "peer")
	f.assignRole(t, own, exec.membershipID, execRole)
	f.assignRole(t, own, peer.membershipID, execRole)
	// the peer holds the route's permission but does not outrank the member
	f.grantOverrides(t, own, peer.membershipID, []string{p.Users.View, p.Users.UpdatePermissions}, nil)
	peerTok := f.loginToken(t, peer.email, peer.password)

	current := f.overridesVersion(t, own, exec.membershipID)
	var raw map[string]json.RawMessage
	g := f.authedJSON(t, http.MethodGet, "/api/v1/users/"+exec.membershipID, peerTok, nil)
	if err := json.Unmarshal(g.body, &raw); err != nil || g.status != http.StatusOK {
		t.Fatalf("the peer reads the member: %d %s (%v)", g.status, g.body, err)
	}
	for _, k := range []string{"permission_overrides", "overrides_version"} {
		if _, shown := raw[k]; shown {
			t.Errorf("a peer is shown %s: %s", k, g.body)
		}
	}
	for _, v := range []string{current, "ov1-a-guess"} {
		r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+exec.membershipID+"/permission-overrides", peerTok,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}, OverridesVersion: v})
		if r.status != http.StatusForbidden || decodeError(t, r.body).Error != "insufficient_rank" {
			t.Errorf("a peer saving with version %q: %d %s, want 403 insufficient_rank (the same answer whether or not the version is right)", v, r.status, r.body)
		}
	}
	if got := f.overridesVersion(t, own, exec.membershipID); got != current {
		t.Errorf("the refused saves changed the member's overrides version: %q -> %q", current, got)
	}
}
