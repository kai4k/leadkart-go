package query_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
)

const callerMembershipID = membership.ID("99999999-9999-9999-9999-999999999999")

// overridesWorld is one tenant with a member (level-50 role) holding a permanent
// grant, a time-bound grant, an expired grant and a revocation.
func overridesWorld(t *testing.T) (query.GetUserOverridesHandler, time.Time) {
	t.Helper()
	return overridesWorldAt(t, func() time.Time { return testNow })
}

// overridesWorldAt is [overridesWorld] read at the given clock.
func overridesWorldAt(t *testing.T, now func() time.Time) (query.GetUserOverridesHandler, time.Time) {
	t.Helper()
	p := newPerson(t)
	m := newMembership(t, testMembershipID, p.ID(), testTenantID)
	r := newRole(t, testRoleID, testTenantID, "Executive")
	if err := m.AssignRole(r.ID(), testNow); err != nil {
		t.Fatal(err)
	}
	id := permission.IdentityPermissions
	expiry := testNow.Add(72 * time.Hour)
	if err := m.ReplacePermissionOverrides([]membership.GrantedOverride{
		{Permission: permission.FromConstant(id.Tasks.WorkItems.Read)},
		{Permission: permission.FromConstant(id.Tasks.WorkItems.Manage), ExpiresAt: expiry},
		{Permission: permission.FromConstant(id.Users.View), ExpiresAt: testNow.Add(-time.Hour)},
	}, []*permission.Permission{permission.FromConstant(id.Roles.View)}, testNow); err != nil {
		t.Fatal(err)
	}
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	return query.NewGetUserOverridesHandler(mems, roles, persons, now), expiry
}

func actorOfRank(t *testing.T, id membership.ID, rank int, super bool) authority.Actor {
	t.Helper()
	a, err := authority.NewActor(authority.ActorInput{MembershipID: id, TenantID: testTenantID, Rank: &rank, IsSuperUser: super})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGetUserOverrides_SeniorCallerSeesLiveOverridesWithExpiry(t *testing.T) {
	t.Parallel()
	h, expiry := overridesWorld(t)
	id := permission.IdentityPermissions
	got, err := h.Handle(t.Context(), query.GetUserOverridesQuery{
		TenantID: testTenantID, MembershipID: testMembershipID, Actor: actorOfRank(t, callerMembershipID, 10, false)})
	if err != nil || !got.Visible {
		t.Fatalf("Handle = %v, visible=%v, want the overrides", err, got.Visible)
	}
	want := query.UserOverridesView{
		Visible: true,
		Granted: []query.OverrideView{
			{Name: id.Tasks.WorkItems.Manage, ExpiresAt: expiry},
			{Name: id.Tasks.WorkItems.Read},
		},
		Revoked: []string{id.Roles.View},
		// the version of exactly the live set shown: what a save must send back
		Version: authority.Overrides{
			Granted: []authority.Override{{Name: id.Tasks.WorkItems.Read}, {Name: id.Tasks.WorkItems.Manage, ExpiresAt: expiry}},
			Revoked: []string{id.Roles.View},
		}.Version(),
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("overrides (-want +got): %s (an expired grant must not be listed; a permanent grant has a zero expiry)", d)
	}
}

func TestGetUserOverrides_CallersWhoMayNotChangeThemSeeNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		actor func(t *testing.T) authority.Actor
	}{
		{"the zero actor", func(*testing.T) authority.Actor { return authority.Actor{} }},
		{"a peer of the same rank", func(t *testing.T) authority.Actor { return actorOfRank(t, callerMembershipID, 50, false) }},
		{"a junior", func(t *testing.T) authority.Actor { return actorOfRank(t, callerMembershipID, 60, false) }},
		{"the member themself", func(t *testing.T) authority.Actor { return actorOfRank(t, testMembershipID, 10, false) }},
		{"a caller with no rank claim", func(t *testing.T) authority.Actor {
			a, err := authority.NewActor(authority.ActorInput{MembershipID: callerMembershipID, TenantID: testTenantID})
			if err != nil {
				t.Fatal(err)
			}
			return a
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := overridesWorld(t)
			got, err := h.Handle(t.Context(), query.GetUserOverridesQuery{
				TenantID: testTenantID, MembershipID: testMembershipID, Actor: c.actor(t)})
			if err != nil {
				t.Fatalf("Handle: %v, want no error (absent, not refused)", err)
			}
			if got.Visible || len(got.Granted) != 0 || len(got.Revoked) != 0 || got.Version != "" {
				t.Errorf("a caller who may not change the overrides got %+v", got)
			}
		})
	}
}

// The version is of the LIVE set, so it changes the moment a time-bound grant
// expires: a form opened before the expiry cannot be saved after it.
func TestGetUserOverrides_TheVersionChangesWhenAGrantExpires(t *testing.T) {
	t.Parallel()
	read := func(at time.Time) query.UserOverridesView {
		h, _ := overridesWorldAt(t, func() time.Time { return at })
		got, err := h.Handle(t.Context(), query.GetUserOverridesQuery{
			TenantID: testTenantID, MembershipID: testMembershipID, Actor: actorOfRank(t, callerMembershipID, 10, false)})
		if err != nil || !got.Visible {
			t.Fatalf("Handle at %v = %v, visible=%v", at, err, got.Visible)
		}
		return got
	}
	before, stillBefore := read(testNow), read(testNow.Add(71*time.Hour))
	after := read(testNow.Add(72 * time.Hour)) // the instant of expiry is expired
	if before.Version == "" || before.Version != stillBefore.Version {
		t.Errorf("versions %q and %q while nothing changed, want the same non-empty version", before.Version, stillBefore.Version)
	}
	if after.Version == before.Version {
		t.Errorf("the version is still %q after the time-bound grant expired (granted now %+v)", after.Version, after.Granted)
	}
	if len(after.Granted) != 1 {
		t.Errorf("granted after the expiry = %+v, want only the permanent grant", after.Granted)
	}
}

func TestGetUserOverrides_AnotherTenantIsNotFound(t *testing.T) {
	t.Parallel()
	h, _ := overridesWorld(t)
	_, err := h.Handle(t.Context(), query.GetUserOverridesQuery{
		TenantID: "aaaaaaaa-0000-0000-0000-000000000001", MembershipID: testMembershipID, Actor: actorOfRank(t, callerMembershipID, 10, false)})
	if err == nil {
		t.Fatal("a membership of another tenant was read")
	}
}

func TestGetUserOverrides_RejectsZeroInputsAndNilDeps(t *testing.T) {
	t.Parallel()
	h, _ := overridesWorld(t)
	if _, err := h.Handle(t.Context(), query.GetUserOverridesQuery{MembershipID: testMembershipID}); err == nil {
		t.Error("zero tenant accepted")
	}
	if _, err := h.Handle(t.Context(), query.GetUserOverridesQuery{TenantID: testTenantID}); err == nil {
		t.Error("zero membership accepted")
	}
	defer func() {
		if recover() == nil {
			t.Error("nil repositories did not panic")
		}
	}()
	_ = query.NewGetUserOverridesHandler(nil, roletest.NewFakeRepository(), persontest.NewFakeRepository(), nil) // arch-test:ignore-err - panic check
}
