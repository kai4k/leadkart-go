package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// Hierarchy round 5, commit 2: the approver queue tells the truth (can_approve),
// costs nothing for a caller who can decide nothing, scans only a manager's own
// reports, and runs in one read transaction; approvals are bounded by the
// approver's own grant and answer 404 to callers who may not even deny.

func (w *prWorld) queueAs(t *testing.T, a authority.Actor) command.ListDecidablePermissionRequestsResult {
	t.Helper()
	res, err := w.queue.Handle(t.Context(), command.ListDecidablePermissionRequestsQuery{TenantID: awTenant, Actor: a})
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	return res
}

// TestApproverQueue_CanApproveIsTheTruth: for every listed request, approving
// succeeds exactly when can_approve says so.
func TestApproverQueue_CanApproveIsTheTruth(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	callers := map[string]func(w *prWorld, c prCast) authority.Actor{
		"team lead holding Read and ReadAll": func(_ *prWorld, c prCast) authority.Actor {
			// a stale token claims ReadAll; the stored facts (the Team Lead role) do not
			// confer it, and the queue decides on the stored facts like approve does
			return actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read, p.Crm.Leads.ReadAll)
		},
		"administrator holding nothing": func(_ *prWorld, c prCast) authority.Actor {
			return actorAs(t, c.admin.ID(), authority.RankAdmin)
		},
		"owner": func(w *prWorld, _ prCast) authority.Actor {
			return actorAs(t, w.member(t, w.owner).ID(), authority.RankOwner, tenantAdminEffective()...)
		},
	}
	for name, mk := range callers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newPRWorld(t)
			c := w.cast(t)
			for _, perm := range []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Roles.Update, p.Users.Create, p.Orders.Payments.Read} {
				w.pending(t, c.exec, perm, 0)
			}
			w.pendingForged(t, c.exec.ID(), p.Meta.TenantAdmin) // stored row, not requestable
			actor := mk(w, c)
			res := w.queueAs(t, actor)
			if len(res.Page.Items) == 0 {
				t.Fatal("empty queue: nothing to compare")
			}
			var yes, no int
			for _, it := range res.Page.Items {
				err := w.approveAs(actor, it.Request)
				switch {
				case it.CanApprove && err != nil:
					t.Errorf("%s listed with can_approve but approve failed: %v", it.Request.Permission().Name(), err)
				case !it.CanApprove && err == nil:
					t.Errorf("%s listed without can_approve but approve succeeded", it.Request.Permission().Name())
				case !it.CanApprove && !errors.Is(err, authority.ErrGrantExceedsAuthority) && !errors.Is(err, authority.ErrAdminTierOnly) && !errors.Is(err, permissionrequest.ErrPermissionNotRequestable): // ErrAdminTierOnly: owner ruling 2026-11-21 (users.create for a rank-50 requester)
					t.Errorf("%s: refused for the wrong reason: %v", it.Request.Permission().Name(), err)
				}
				if it.CanApprove {
					yes++
				} else {
					no++
				}
			}
			if name != "owner" && (yes == 0 || no == 0) {
				t.Errorf("the scenario must list both kinds: %d approvable, %d not", yes, no)
			}
		})
	}
}

// A caller who can decide nothing gets an empty page without a single read.
func TestApproverQueue_ShortCircuitsWhenTheCallerCanDecideNothing(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	for range 20 {
		w.pending(t, w.member(t, w.exec), p.Crm.Leads.Read, 0)
	}
	var txs int
	n := tallyCounts{}
	queue := command.NewListDecidablePermissionRequestsHandler(txCounter{n: &txs}, w.reqs, tallyMembers{Repository: w.members, n: n}, tallyRoles{Repository: w.roles, n: n}, tallyPersons{Repository: anyPersons{}, n: n}, awClock)
	noApprove := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Crm.Leads.Read) // a manager without roles.approve
	exec := actorAs(t, c.other.ID(), authority.RankUser, p.Crm.Leads.Read)
	for name, a := range map[string]authority.Actor{"team lead without roles.approve": noApprove, "executive": exec} {
		res, err := queue.Handle(t.Context(), command.ListDecidablePermissionRequestsQuery{TenantID: awTenant, Actor: a})
		if err != nil || res.Page.Items == nil || len(res.Page.Items) != 0 || res.Page.HasMore {
			t.Errorf("%s: %+v %v, want an empty non-nil page", name, res.Page, err)
		}
	}
	if w.reqs.ListPendingCalls() != 0 || txs != 0 || len(n) != 0 {
		t.Errorf("a caller who can decide nothing cost %d list calls, %d transactions and %v reads, want none", w.reqs.ListPendingCalls(), txs, n)
	}
}

type txCounter struct{ n *int }

func (u txCounter) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	*u.n++
	return fn(ctx)
}

// A manager below the Admin tier is shown only their own reports' requests: the
// window is not spent on requests of strangers, however many there are.
func TestApproverQueue_ManagerScansOnlyTheirReports(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	// 130 newer requests from members who do NOT report to the team lead
	var strangers []*membership.Membership
	for range 130 {
		strangers = append(strangers, w.member(t, w.exec))
	}
	mine := w.pending(t, c.exec, p.Crm.Leads.Read, 0)
	for _, s := range strangers {
		w.pending(t, s, p.Crm.Leads.Read, 0)
	}
	tl := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)
	res := w.queueAs(t, tl)
	if len(res.Page.Items) != 1 || res.Page.Items[0].Request.ID() != mine.ID() || !res.Page.Items[0].CanApprove {
		t.Fatalf("team lead's queue = %d items, want exactly their report's request (it would be starved behind 130 others otherwise)", len(res.Page.Items))
	}
	// the Administrator is not filtered
	admin := actorAs(t, c.admin.ID(), authority.RankAdmin)
	full, err := w.queue.Handle(t.Context(), command.ListDecidablePermissionRequestsQuery{TenantID: awTenant, Actor: admin, PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(full.Page.Items); got != 100 {
		t.Errorf("admin's window = %d items, want the full window of 100", got)
	}
}

// One read transaction, and a cost that does not depend on the number of
// requests, only on the number of distinct requesters.
func TestApproverQueue_RunsInOneTransactionWithBoundedReads(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	run := func(requesters, perRequester int) (txs int, n tallyCounts) {
		w := newPRWorld(t)
		c := w.cast(t)
		n = tallyCounts{}
		queue := command.NewListDecidablePermissionRequestsHandler(txCounter{n: &txs}, w.reqs, tallyMembers{Repository: w.members, n: n}, tallyRoles{Repository: w.roles, n: n}, tallyPersons{Repository: anyPersons{}, n: n}, awClock)
		for range requesters {
			m := w.member(t, w.exec)
			for _, perm := range []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign}[:perRequester] {
				w.pending(t, m, perm, 0)
			}
		}
		admin := actorAs(t, c.admin.ID(), authority.RankAdmin)
		res, err := queue.Handle(t.Context(), command.ListDecidablePermissionRequestsQuery{TenantID: awTenant, Actor: admin})
		if err != nil || len(res.Page.Items) != requesters*perRequester {
			t.Fatalf("queue = %d items, %v; want %d", len(res.Page.Items), err, requesters*perRequester)
		}
		return txs, n
	}
	txs, small := run(5, 1)
	txsMore, many := run(5, 3) // three times the requests, same requesters
	if txs != 1 || txsMore != 1 {
		t.Errorf("transactions = %d and %d, want 1 (one read transaction)", txs, txsMore)
	}
	// the caller is re-read once (membership, roles, person) before the window is read
	if small["members.GetByID"] != 1 || many["members.GetByID"] != 1 || small["members.GetByIDs"] != 1 || many["members.GetByIDs"] != 1 {
		t.Errorf("membership reads = %d single + %d batch and %d single + %d batch, want the caller once and ONE batch for all requesters", small["members.GetByID"], small["members.GetByIDs"], many["members.GetByID"], many["members.GetByIDs"])
	}
	for name, want := range map[string]int{"roles.GetByIDsIncludingInactive": 2, "persons.GetByIDs": 1, "persons.GetByID": 1} {
		if small[name] != want || many[name] != want {
			t.Errorf("%s = %d and %d, want %d (a fixed number: the caller's own read, and one batch read for all requesters)", name, small[name], many[name], want)
		}
	}
}

// An approval never outlives the approver's own grant.
func TestApprove_ExpiryIsCappedAtTheApproversOwnGrant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	soon := testNow.Add(48 * time.Hour)
	for _, tc := range []struct {
		name      string
		approver  []membership.GrantedOverride
		roleHolds bool
		want      time.Time
	}{
		{"held only through a 2-day override", []membership.GrantedOverride{{Permission: permission.FromConstant(p.Crm.Leads.Assign), ExpiresAt: soon}}, false, soon},
		{"held through a permanent override", []membership.GrantedOverride{{Permission: permission.FromConstant(p.Crm.Leads.Assign)}}, false, testNow.Add(90 * 24 * time.Hour)},
		{"held through a role", nil, true, testNow.Add(90 * 24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newPRWorld(t)
			perms := []string{p.Roles.Approve}
			if tc.roleHolds {
				perms = append(perms, p.Crm.Leads.Assign)
			}
			lead := w.customRole(t, "Team Lead plus", 30, perms...)
			tl := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{lead.ID()}, GrantedPermissions: tc.approver})
			exec := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID()}, ReportsTo: tl.ID()})
			req := w.pending(t, exec, p.Crm.Leads.Assign, 90)
			actor := actorAs(t, tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Assign)
			if err := w.approveAs(actor, req); err != nil {
				t.Fatalf("approve: %v", err)
			}
			g, ok := grantOf(exec, p.Crm.Leads.Assign)
			if !ok || !g.ExpiresAt.Equal(tc.want) {
				t.Errorf("grant = %+v (found %v), want it to expire at %v", g, ok, tc.want)
			}
			got, _ := w.reqs.GetByID(t.Context(), awTenant, req.ID())
			if !got.ExpiresAt().Equal(tc.want) {
				t.Errorf("the request records expiry %v, want the capped %v", got.ExpiresAt(), tc.want)
			}
		})
	}
}

// A requester whose roles cannot be loaded is a 409 for a caller who could
// decide, and the same 404 as everything else for one who could not.
func TestApprove_InconsistentRequesterIsNotAnOracleForIneligibleCallers(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	broken := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID(), role.ID(awID(8888))}, ReportsTo: c.tl.ID()})
	req := w.pending(t, broken, p.Crm.Leads.Read, 0)
	admin := actorAs(t, c.admin.ID(), authority.RankAdmin)
	stranger := actorAs(t, c.other.ID(), authority.RankUser, p.Crm.Leads.Read)
	if err := w.approveAs(admin, req); !errors.Is(err, command.ErrMemberRolesInconsistent) {
		t.Errorf("an admin approves for a requester with inconsistent roles: %v, want ErrMemberRolesInconsistent", err)
	}
	if err := w.approveAs(stranger, req); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("a stranger: %v, want ErrPermissionRequestNotFound", err)
	}
	if err := w.denyAs(stranger, req); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("a stranger denies: %v, want ErrPermissionRequestNotFound", err)
	}
}
