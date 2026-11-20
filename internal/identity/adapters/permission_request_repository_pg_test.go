//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
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

	page, err := reqs.ListPendingInTenant(ctx, tn.ID(), 2, pagination.Cursor{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ID() != newest.ID() || page.Items[1].ID() != middle.ID() || !page.HasMore {
		t.Fatalf("first page = %v (has_more %v), want [newest, middle] and more", ids3(page), page.HasMore)
	}
	next, err := reqs.ListPendingInTenant(ctx, tn.ID(), 2, decodeCursor(t, page.NextCursor))
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].ID() != oldest.ID() || next.HasMore {
		t.Fatalf("second page = %v (has_more %v), want [oldest] and no more", ids3(next), next.HasMore)
	}
	// RLS: the other tenant's scope sees only its own pending request.
	otherCtx := tenancy.WithID(t.Context(), tenancy.ID(other.ID().String()))
	theirs, err := reqs.ListPendingInTenant(otherCtx, other.ID(), 10, pagination.Cursor{})
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
