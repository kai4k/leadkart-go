//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//
// arch-test:no-synctest - the row-lock test crosses two real Postgres
//   transactions; a virtual clock cannot order a lock wait.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via tenancy.WithID(); RLS isolates
//   rows by tenant so parallel runs cannot see each others state.
//
// SQL-CONTRACT COVERAGE for this file: ListPendingInTenant (the approver
// queue's source, ADR 0069 A6) returns only the tenant's PENDING requests,
// newest first, keyset-paginated, and never another tenant's.

package adapters_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestPermissionRequestRepository_ListPendingInTenant_PendingOnlyNewestFirstKeyset(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)
	reqs := adapters.NewPermissionRequestRepository(pool, tx)

	tn, other := seedTenant(t, tenants), seedTenant(t, tenants)
	member := func(tid tenant.ID, email string) membership.ID {
		p := seedPerson(t, persons, email)
		m, err := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tid, membership.ID(""), testNow)
		if err != nil {
			t.Fatalf("membership.New: %v", err)
		}
		if err := memberships.Add(tenancy.WithID(t.Context(), tenancy.ID(tid.String())), m); err != nil {
			t.Fatalf("Add membership: %v", err)
		}
		return m.ID()
	}
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	requester := member(tn.ID(), "queue-a@example.test")
	otherRequester := member(other.ID(), "queue-b@example.test")

	file := func(tid tenant.ID, who membership.ID, perm string, at time.Time) *permissionrequest.Request {
		r, err := permissionrequest.New(permissionrequest.ID(ids.NewV7().String()), tid, who, permission.FromConstant(perm), 7, "needed for the quarterly close", at)
		if err != nil {
			t.Fatalf("permissionrequest.New: %v", err)
		}
		if err := reqs.Add(tenancy.WithID(t.Context(), tenancy.ID(tid.String())), r); err != nil {
			t.Fatalf("Add request: %v", err)
		}
		return r
	}
	p := permission.IdentityPermissions
	oldest := file(tn.ID(), requester, p.Crm.Leads.Read, testNow)
	middle := file(tn.ID(), requester, p.Crm.Leads.ReadAll, testNow.Add(time.Minute))
	newest := file(tn.ID(), requester, p.Crm.Leads.Assign, testNow.Add(2*time.Minute))
	decided := file(tn.ID(), requester, p.Inventory.Stock.Read, testNow.Add(3*time.Minute))
	file(other.ID(), otherRequester, p.Crm.Leads.Read, testNow.Add(4*time.Minute))
	if err := reqs.UpdateByID(ctx, tn.ID(), decided.ID(), func(r *permissionrequest.Request) (bool, error) {
		return true, r.Cancel(testNow.Add(5 * time.Minute))
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	page, err := reqs.ListPendingInTenant(ctx, tn.ID(), "", 2, pagination.Cursor{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ID() != newest.ID() || page.Items[1].ID() != middle.ID() || !page.HasMore {
		t.Fatalf("first page = %v (has_more %v), want [newest, middle] and more", ids3(page), page.HasMore)
	}
	next, err := reqs.ListPendingInTenant(ctx, tn.ID(), "", 2, decodeCursor(t, page.NextCursor))
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].ID() != oldest.ID() || next.HasMore {
		t.Fatalf("second page = %v (has_more %v), want [oldest] and no more", ids3(next), next.HasMore)
	}
	// RLS: the other tenant's scope sees only its own pending request.
	otherCtx := tenancy.WithID(t.Context(), tenancy.ID(other.ID().String()))
	theirs, err := reqs.ListPendingInTenant(otherCtx, other.ID(), "", 10, pagination.Cursor{})
	if err != nil || len(theirs.Items) != 1 || theirs.Items[0].TenantID() != other.ID() {
		t.Fatalf("other tenant's queue = %v, %v; want exactly its own request", ids3(theirs), err)
	}
}

func ids3(p pagination.Page[*permissionrequest.Request]) []string {
	out := make([]string, 0, len(p.Items))
	for _, r := range p.Items {
		out = append(out, r.ID().String())
	}
	return out
}

func decodeCursor(t *testing.T, raw string) pagination.Cursor {
	t.Helper()
	c, err := pagination.Decode(raw)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	return c
}

// SQL-contract: reportsTo keeps only the requests of members whose manager is
// that membership, and (with the zero id) the whole tenant is listed; the
// filter is in the query, so a window is never spent on other members' requests.
func TestPermissionRequestRepository_ListPendingInTenant_ReportsToFilter(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)
	reqs := adapters.NewPermissionRequestRepository(pool, tx)
	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	member := func(email string, manager membership.ID) membership.ID {
		pe := seedPerson(t, persons, email)
		m, err := membership.New(membership.ID(ids.NewV7().String()), pe.ID(), tn.ID(), "", testNow)
		if err != nil {
			t.Fatalf("membership.New: %v", err)
		}
		if err := memberships.Add(ctx, m); err != nil {
			t.Fatalf("Add membership: %v", err)
		}
		if manager != "" {
			if err := memberships.UpdateByID(ctx, tn.ID(), m.ID(), func(mm *membership.Membership) (bool, error) {
				return true, mm.AssignManager(manager, testNow)
			}); err != nil {
				t.Fatalf("assign manager: %v", err)
			}
		}
		return m.ID()
	}
	boss := member("rt-boss@example.test", "")
	mine := member("rt-mine@example.test", boss)
	stranger := member("rt-stranger@example.test", "")
	file := func(who membership.ID, at time.Time) *permissionrequest.Request {
		r, err := permissionrequest.New(permissionrequest.ID(ids.NewV7().String()), tn.ID(), who, permission.FromConstant(permission.IdentityPermissions.Crm.Leads.Read), 7, "needed for the quarterly close", at)
		if err != nil {
			t.Fatalf("permissionrequest.New: %v", err)
		}
		if err := reqs.Add(ctx, r); err != nil {
			t.Fatalf("Add request: %v", err)
		}
		return r
	}
	want := file(mine, testNow)
	file(stranger, testNow.Add(time.Minute))

	got, err := reqs.ListPendingInTenant(ctx, tn.ID(), boss, 10, pagination.Cursor{})
	if err != nil || len(got.Items) != 1 || got.Items[0].ID() != want.ID() {
		t.Fatalf("boss's queue = %v, %v; want only their report's request", ids3(got), err)
	}
	all, err := reqs.ListPendingInTenant(ctx, tn.ID(), "", 10, pagination.Cursor{})
	if err != nil || len(all.Items) < 2 {
		t.Fatalf("whole queue = %v, %v; want every pending request", ids3(all), err)
	}
	none, err := reqs.ListPendingInTenant(ctx, tn.ID(), mine, 10, pagination.Cursor{})
	if err != nil || len(none.Items) != 0 {
		t.Fatalf("a member without reports has a queue of %v, %v; want empty", ids3(none), err)
	}
}

// Race (hierarchy round 6, I2): two writers of one request serialise on its row.
// The cancel is held between its read and its write while an approval tries to
// commit in that gap. Without the row lock the approval commits and the cancel
// then overwrites it from its stale snapshot (cancelled, approver wiped, while
// the approval answered success). With the lock the approval waits for the
// cancel and finds it no longer pending. Either way what each caller was told
// must be what is stored.
func TestPermissionRequestRepository_UpdateByID_WritersSerialiseOnTheRow(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)
	reqs := adapters.NewPermissionRequestRepository(pool, tx)
	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	member := func(email string) membership.ID {
		pe := seedPerson(t, persons, email)
		m, err := membership.New(membership.ID(ids.NewV7().String()), pe.ID(), tn.ID(), "", testNow)
		if err != nil {
			t.Fatalf("membership.New: %v", err)
		}
		if err := memberships.Add(ctx, m); err != nil {
			t.Fatalf("Add membership: %v", err)
		}
		return m.ID()
	}
	requester, approver := member("race-requester@example.test"), member("race-approver@example.test")
	r, err := permissionrequest.New(permissionrequest.ID(ids.NewV7().String()), tn.ID(), requester, permission.FromConstant(permission.IdentityPermissions.Crm.Leads.Read), 7, "needed for the quarterly close", testNow)
	if err != nil {
		t.Fatalf("permissionrequest.New: %v", err)
	}
	if err := reqs.Add(ctx, r); err != nil {
		t.Fatalf("Add request: %v", err)
	}

	loaded, approvalDone := make(chan struct{}), make(chan struct{})
	cancelErr := make(chan error, 1)
	go func() {
		cancelErr <- reqs.UpdateByID(ctx, tn.ID(), r.ID(), func(loadedReq *permissionrequest.Request) (bool, error) {
			close(loaded) // the cancel has read the request as pending
			select {
			case <-approvalDone: // the approval committed in the gap (no row lock)
			case <-time.After(2 * time.Second): // the approval is waiting for this transaction (row lock)
			}
			return true, loadedReq.Cancel(testNow.Add(time.Minute))
		})
	}()
	<-loaded
	approveErr := reqs.UpdateByID(ctx, tn.ID(), r.ID(), func(loadedReq *permissionrequest.Request) (bool, error) {
		return true, loadedReq.Approve(approver, "go ahead", ids.NewV7(), testNow.Add(24*time.Hour), testNow.Add(time.Minute))
	})
	close(approvalDone)
	cancelResult := <-cancelErr

	got, err := reqs.GetByID(ctx, tn.ID(), r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	switch got.State() {
	case permissionrequest.StateApproved:
		if cancelResult == nil || approveErr != nil || got.ApproverMembershipID() != approver {
			t.Fatalf("stored approved (approver %q) but approve answered %v and cancel answered %v", got.ApproverMembershipID(), approveErr, cancelResult)
		}
	case permissionrequest.StateCancelled:
		if cancelResult != nil || approveErr == nil {
			t.Fatalf("stored cancelled but cancel answered %v and approve answered %v: an approval that said success was erased", cancelResult, approveErr)
		}
		if got.ApproverMembershipID() != "" {
			t.Fatalf("cancelled request still carries approver %q", got.ApproverMembershipID())
		}
	default:
		t.Fatalf("request ended %s", got.State())
	}
}
