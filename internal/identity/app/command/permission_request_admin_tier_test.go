package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
)

// Owner ruling 2026-11-21: identity.users.create is held and conferred only at
// the Administrator tier or above, so it cannot be requested below that tier
// and a pending request for it cannot be approved for such a member.

func TestRequestPermissionElevation_UsersCreateIsAskedOnlyAtTheAdministratorTier(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	for _, tc := range []struct {
		name string
		rank authority.Rank
		perm string
		want error
	}{
		{"a manager asks for it", authority.RankManager, p.Users.Create, authority.ErrAdminTierOnly},
		{"a team lead asks for it", authority.RankTeamLead, p.Users.Create, authority.ErrAdminTierOnly},
		{"an executive asks for it", authority.RankUser, p.Users.Create, authority.ErrAdminTierOnly},
		{"a member with no role asks for it", authority.RankNone, p.Users.Create, authority.ErrAdminTierOnly},
		{"a manager asks for another name", authority.RankManager, p.Users.Update, nil},
		{"an administrator asks for it", authority.RankAdmin, p.Users.Create, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reqs, mems := newFakePermissionRequestRepo(), newFakeMembershipRepoForPermReq()
			requester := freshMembershipForPermReq(t)
			_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
			h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
			_, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
				Actor:                 actorAs(t, requester.ID(), tc.rank, p.Meta.RequestPermissionElevation),
				TenantID:              requester.TenantID(),
				RequesterMembershipID: requester.ID(),
				Permission:            permission.FromConstant(tc.perm),
				Reason:                "need to onboard 5 users for monthly sales drive",
			})
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Handle: %v, want %v", err, tc.want)
			}
		})
	}
}

// A stale pending request for it (stored before the ruling) is never approved
// for a requester below the tier: not by an Administrator, not by an owner, and
// it stays pending (it can still be denied or cancelled).
func TestApprove_UsersCreateIsNotApprovedBelowTheAdministratorTier(t *testing.T) {
	t.Parallel()
	create := permission.IdentityPermissions.Users.Create
	for who, mk := range map[string]func(w *prWorld, c prCast) authority.Actor{
		"an administrator": func(_ *prWorld, c prCast) authority.Actor {
			return actorAs(t, c.admin.ID(), authority.RankAdmin, tenantAdminEffective()...)
		},
		"an owner": func(w *prWorld, _ prCast) authority.Actor {
			return actorAs(t, w.member(t, w.owner).ID(), authority.RankOwner, tenantAdminEffective()...)
		},
	} {
		t.Run(who, func(t *testing.T) {
			t.Parallel()
			w := newPRWorld(t)
			c := w.cast(t)
			for name, requester := range map[string]*prRequester{"an executive": {c.exec}, "a manager": {c.mgr}, "a team lead": {c.tl}} {
				req := w.pending(t, requester.m, create, 0)
				err := w.approveAs(mk(w, c), req)
				if !errors.Is(err, authority.ErrAdminTierOnly) {
					t.Errorf("approving it for %s: %v, want ErrAdminTierOnly", name, err)
				}
				if _, held := grantOf(requester.m, create); held {
					t.Errorf("%s was granted it", name)
				}
				if got, _ := w.reqs.GetByID(t.Context(), awTenant, req.ID()); got.State() != permissionrequest.StatePending {
					t.Errorf("the request for %s is %s, want it left pending", name, got.State())
				}
			}
		})
	}
}

type prRequester struct{ m *membership.Membership }
