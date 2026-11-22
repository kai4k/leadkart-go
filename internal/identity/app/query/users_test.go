package query_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- GetUserHandler ------------------------------------------------------

func TestNewGetUserHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	mems := membershiptest.NewFakeRepository()
	persons := persontest.NewFakeRepository()
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil memberships", func() {
			_ = query.NewGetUserHandler(nil, persons) // arch-test:ignore-err - test fixture setup
		}},
		{"nil persons", func() {
			_ = query.NewGetUserHandler(mems, nil) // arch-test:ignore-err - test fixture setup
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic")
				}
			}()
			c.fn()
		})
	}
}

func TestGetUser_RejectsZeroInputs(t *testing.T) {
	t.Parallel()
	h := query.NewGetUserHandler(membershiptest.NewFakeRepository(), persontest.NewFakeRepository())
	cases := []struct {
		name string
		q    query.GetUserQuery
	}{
		{"zero tenant", query.GetUserQuery{MembershipID: testMembershipID}},
		{"zero membership", query.GetUserQuery{TenantID: testTenantID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(t.Context(), c.q)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGetUser_MembershipNotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetUserHandler(membershiptest.NewFakeRepository(), persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetUserQuery{TenantID: testTenantID, MembershipID: testMembershipID})
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("err = %v, want membership.ErrNotFound", err)
	}
}

func TestGetUser_PersonNotFound(t *testing.T) {
	t.Parallel()
	m := newMembership(t, testMembershipID, testPersonID, testTenantID)
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	persons := persontest.NewFakeRepository()
	h := query.NewGetUserHandler(mems, persons)
	_, err := h.Handle(t.Context(), query.GetUserQuery{TenantID: testTenantID, MembershipID: testMembershipID})
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

func TestGetUser_HappyPath_ComposesAllFields(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	m := newMembership(t, testMembershipID, p.ID(), testTenantID)
	if err := m.UpdateProfile("Sales Lead", "Sales", "OOO", testNow); err != nil {
		t.Fatal(err)
	}
	mgr := membership.ID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err := m.AssignManager(mgr, testNow); err != nil {
		t.Fatal(err)
	}
	if err := m.AssignRole(testRoleID, testNow); err != nil {
		t.Fatal(err)
	}

	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	h := query.NewGetUserHandler(mems, persons)
	got, err := h.Handle(t.Context(), query.GetUserQuery{TenantID: testTenantID, MembershipID: m.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := query.UserView{
		MembershipID:  m.ID().String(),
		PersonID:      p.ID().String(),
		TenantID:      testTenantID.String(),
		Email:         testEmail,
		FirstName:     "Alice",
		LastName:      "Liddell",
		Status:        membership.StatusActive.String(),
		Designation:   "Sales Lead",
		Department:    "Sales",
		StatusMessage: "OOO",
		JoinedAt:      m.JoinedAt().UTC(),
		LeftAt:        time.Time{},
		ReportsTo:     mgr.String(),
		RoleIDs:       []string{testRoleID.String()},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("UserView (-want +got)\n%s", diff)
	}
}

// ----- ListUsersHandler ----------------------------------------------------

func TestNewListUsersHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	mems := membershiptest.NewFakeRepository()
	persons := persontest.NewFakeRepository()
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil memberships", func() {
			_ = query.NewListUsersHandler(nil, persons) // arch-test:ignore-err - test fixture setup
		}},
		{"nil persons", func() {
			_ = query.NewListUsersHandler(mems, nil) // arch-test:ignore-err - test fixture setup
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic")
				}
			}()
			c.fn()
		})
	}
}

func TestListUsers_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	h := query.NewListUsersHandler(membershiptest.NewFakeRepository(), persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListUsersQuery{})
	if err == nil {
		t.Fatal("expected error on zero tenant")
	}
}

// membershipsErrRepo injects failures on listing methods.
type membershipsErrRepo struct {
	membership.Repository
	listErr     error
	listPageErr error
}

func (r membershipsErrRepo) ListForTenant(ctx context.Context, tID tenant.ID) ([]*membership.Membership, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.Repository.ListForTenant(ctx, tID)
}

func (r membershipsErrRepo) ListForTenantPage(ctx context.Context, tID tenant.ID, before time.Time, beforeID string, limit int) ([]*membership.Membership, error) {
	if r.listPageErr != nil {
		return nil, r.listPageErr
	}
	return r.Repository.ListForTenantPage(ctx, tID, before, beforeID, limit)
}

func (r membershipsErrRepo) ListForTenantPageByStatus(ctx context.Context, tID tenant.ID, f membership.StatusFilter, before time.Time, beforeID string, limit int) ([]*membership.Membership, error) {
	if r.listPageErr != nil {
		return nil, r.listPageErr
	}
	return r.Repository.ListForTenantPageByStatus(ctx, tID, f, before, beforeID, limit)
}

func TestListUsers_PropagatesListError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list boom")
	mems := membershipsErrRepo{Repository: membershiptest.NewFakeRepository(), listErr: sentinel}
	h := query.NewListUsersHandler(mems, persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListUsersQuery{TenantID: testTenantID})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// personsErrRepo injects failure on the batch-hydration path.
type personsErrRepo struct {
	person.Repository
	getByIDsErr error
}

func (r personsErrRepo) GetByIDs(ctx context.Context, ids []person.ID) (map[person.ID]*person.Person, error) {
	if r.getByIDsErr != nil {
		return nil, r.getByIDsErr
	}
	return r.Repository.GetByIDs(ctx, ids)
}

func TestListUsers_PropagatesHydrateError(t *testing.T) {
	t.Parallel()
	mems := membershiptest.NewFakeRepository()
	sentinel := errors.New("hydrate boom")
	persons := personsErrRepo{Repository: persontest.NewFakeRepository(), getByIDsErr: sentinel}
	h := query.NewListUsersHandler(mems, persons)
	_, err := h.Handle(t.Context(), query.ListUsersQuery{TenantID: testTenantID})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListUsers_PersonAbsentDuringHydrate_Returns404(t *testing.T) {
	t.Parallel()
	// Membership references a Person not in the repo (race-with-soft-delete simulation).
	m := newMembership(t, testMembershipID, testPersonID, testTenantID)
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	persons := persontest.NewFakeRepository() // empty — Person absent
	h := query.NewListUsersHandler(mems, persons)
	_, err := h.Handle(t.Context(), query.ListUsersQuery{TenantID: testTenantID})
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

func TestListUsers_HappyPath_HydratesAllRows(t *testing.T) {
	t.Parallel()
	// Seed two memberships + two persons; verify both are hydrated.
	p1 := newPersonAt(t, person.ID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), "p1@example.test", "Person", "One")
	p2 := newPersonAt(t, person.ID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), "p2@example.test", "Person", "Two")
	m1 := newMembership(t, membership.ID("cccccccc-cccc-cccc-cccc-cccccccccccc"), p1.ID(), testTenantID)
	m2 := newMembership(t, membership.ID("dddddddd-dddd-dddd-dddd-dddddddddddd"), p2.ID(), testTenantID)

	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m1); err != nil {
		t.Fatal(err)
	}
	if err := mems.Add(t.Context(), m2); err != nil {
		t.Fatal(err)
	}
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p1); err != nil {
		t.Fatal(err)
	}
	if err := persons.Add(t.Context(), p2); err != nil {
		t.Fatal(err)
	}

	h := query.NewListUsersHandler(mems, persons)
	got, err := h.Handle(t.Context(), query.ListUsersQuery{TenantID: testTenantID})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, v := range got {
		if v.MembershipID == "" || v.PersonID == "" || v.Email == "" {
			t.Errorf("missing field: %+v", v)
		}
	}
}

// ----- ListUsersPagedHandler -----------------------------------------------

func TestNewListUsersPagedHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	mems := membershiptest.NewFakeRepository()
	persons := persontest.NewFakeRepository()
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil memberships", func() {
			_ = query.NewListUsersPagedHandler(nil, persons) // arch-test:ignore-err - test fixture setup
		}},
		{"nil persons", func() {
			_ = query.NewListUsersPagedHandler(mems, nil) // arch-test:ignore-err - test fixture setup
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic")
				}
			}()
			c.fn()
		})
	}
}

func TestListUsersPaged_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	h := query.NewListUsersPagedHandler(membershiptest.NewFakeRepository(), persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListUsersPagedQuery{})
	if err == nil {
		t.Fatal("expected error on zero tenant")
	}
}

func TestListUsersPaged_PropagatesListError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("page boom")
	mems := membershipsErrRepo{Repository: membershiptest.NewFakeRepository(), listPageErr: sentinel}
	h := query.NewListUsersPagedHandler(mems, persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListUsersPagedQuery{TenantID: testTenantID})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListUsersPaged_PropagatesHydrateError(t *testing.T) {
	t.Parallel()
	mems := membershiptest.NewFakeRepository()
	// Seed one row so the page query returns IDs the hydrate path consumes.
	p := newPerson(t)
	m := newMembership(t, testMembershipID, p.ID(), testTenantID)
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("hydrate boom")
	persons := personsErrRepo{Repository: persontest.NewFakeRepository(), getByIDsErr: sentinel}
	h := query.NewListUsersPagedHandler(mems, persons)
	_, err := h.Handle(t.Context(), query.ListUsersPagedQuery{TenantID: testTenantID, PageSize: 10})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListUsersPaged_PersonAbsent_Returns404(t *testing.T) {
	t.Parallel()
	m := newMembership(t, testMembershipID, testPersonID, testTenantID)
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	h := query.NewListUsersPagedHandler(mems, persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListUsersPagedQuery{TenantID: testTenantID, PageSize: 10})
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

func TestListUsersPaged_HappyPath_FirstPageSentinelAdmitsAllRows(t *testing.T) {
	t.Parallel()
	persons := persontest.NewFakeRepository()
	mems := membershiptest.NewFakeRepository()
	for i := range 3 {
		pid := person.ID(fmt.Sprintf("11111111-0000-0000-0000-00000000000%d", i))
		p := newPersonAt(t, pid, fmt.Sprintf("p%d@example.test", i), "P", "Q")
		if err := persons.Add(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		// Stagger joinedAt so paging is deterministic.
		mid := membership.ID(fmt.Sprintf("22222222-0000-0000-0000-00000000000%d", i))
		m, err := membership.New(mid, p.ID(), testTenantID, membership.ID(""), testNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := mems.Add(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	h := query.NewListUsersPagedHandler(mems, persons)
	page, err := h.Handle(t.Context(), query.ListUsersPagedQuery{
		TenantID: testTenantID,
		PageSize: 10, // larger than result set — no next cursor
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 3 {
		t.Errorf("items = %d, want 3", len(page.Items))
	}
	if page.HasMore {
		t.Errorf("HasMore = true, want false")
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty", page.NextCursor)
	}
}

func TestListUsersPaged_PageBoundary_EmitsNextCursor(t *testing.T) {
	t.Parallel()
	persons := persontest.NewFakeRepository()
	mems := membershiptest.NewFakeRepository()
	for i := range 3 {
		pid := person.ID(fmt.Sprintf("11111111-0000-0000-0000-00000000000%d", i))
		p := newPersonAt(t, pid, fmt.Sprintf("p%d@example.test", i), "P", "Q")
		if err := persons.Add(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		mid := membership.ID(fmt.Sprintf("22222222-0000-0000-0000-00000000000%d", i))
		m, err := membership.New(mid, p.ID(), testTenantID, membership.ID(""), testNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := mems.Add(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	h := query.NewListUsersPagedHandler(mems, persons)
	page, err := h.Handle(t.Context(), query.ListUsersPagedQuery{
		TenantID: testTenantID,
		PageSize: 2, // less than 3 results → HasMore=true
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("items = %d, want 2", len(page.Items))
	}
	if !page.HasMore {
		t.Errorf("HasMore = false, want true")
	}
	if page.NextCursor == "" {
		t.Errorf("NextCursor empty; want non-empty")
	}
}

func TestListUsersPaged_ClampZeroPageSize(t *testing.T) {
	t.Parallel()
	// PageSize=0 is clamped to DefaultPageSize internally.
	mems := membershiptest.NewFakeRepository()
	h := query.NewListUsersPagedHandler(mems, persontest.NewFakeRepository())
	page, err := h.Handle(t.Context(), query.ListUsersPagedQuery{TenantID: testTenantID, PageSize: 0})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if page.HasMore {
		t.Errorf("empty repo HasMore=true")
	}
}

func TestListUsersPaged_HonoursCursor(t *testing.T) {
	t.Parallel()
	persons := persontest.NewFakeRepository()
	mems := membershiptest.NewFakeRepository()
	// Stagger joinedAts so cursor predicate cuts cleanly.
	for i := range 3 {
		pid := person.ID(fmt.Sprintf("11111111-0000-0000-0000-00000000000%d", i))
		p := newPersonAt(t, pid, fmt.Sprintf("p%d@example.test", i), "P", "Q")
		if err := persons.Add(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		mid := membership.ID(fmt.Sprintf("22222222-0000-0000-0000-00000000000%d", i))
		m, err := membership.New(mid, p.ID(), testTenantID, membership.ID(""), testNow.Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := mems.Add(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	// Cursor at the second joinedAt — should return the row joined before it.
	cursor := pagination.Cursor{
		SortValue: testNow.Add(1 * time.Hour),
		ID:        "33333333-0000-0000-0000-00000000000z", // > any seeded ID lexicographically
	}
	h := query.NewListUsersPagedHandler(mems, persons)
	page, err := h.Handle(t.Context(), query.ListUsersPagedQuery{
		TenantID: testTenantID,
		Cursor:   cursor,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// joinedAt[0] strictly before cursor → admitted.
	// joinedAt[1] equal but cursor ID lexicographically greater → also admitted.
	if got, want := len(page.Items), 2; got != want {
		t.Errorf("items = %d, want %d", got, want)
	}
}

var _ = role.HierarchyLevelDefault // keep roletest import alive

// ----- ?status= listing + counts --------------------------------------------

// statusWorld seeds two active members and one deactivated one in the test tenant,
// plus one active member of another tenant that no listing may show.
func statusWorld(t *testing.T) (*membershiptest.FakeRepository, *persontest.FakeRepository, map[string]membership.ID) {
	t.Helper()
	persons := persontest.NewFakeRepository()
	mems := membershiptest.NewFakeRepository()
	ids := map[string]membership.ID{}
	add := func(key string, i int, tenantID tenant.ID, deactivate bool) {
		pid := person.ID(fmt.Sprintf("33333333-0000-0000-0000-00000000000%d", i))
		if err := persons.Add(t.Context(), newPersonAt(t, pid, fmt.Sprintf("s%d@example.test", i), "S", "T")); err != nil {
			t.Fatal(err)
		}
		mid := membership.ID(fmt.Sprintf("44444444-0000-0000-0000-00000000000%d", i))
		m, err := membership.New(mid, pid, tenantID, membership.ID(""), testNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if deactivate {
			if err := m.Deactivate("left the company", testNow.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if err := mems.Add(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		ids[key] = mid
	}
	add("a1", 1, testTenantID, false)
	add("a2", 2, testTenantID, false)
	add("gone", 3, testTenantID, true)
	add("foreign", 4, tenant.ID("99999999-9999-9999-9999-999999999999"), false)
	return mems, persons, ids
}

func TestListUsersPaged_StatusFilter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status membership.StatusFilter
		want   []string
	}{
		{"default is active", "", []string{"a2", "a1"}},
		{"active", membership.FilterActive, []string{"a2", "a1"}},
		{"inactive", membership.FilterInactive, []string{"gone"}},
		{"all", membership.FilterAll, []string{"gone", "a2", "a1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mems, persons, ids := statusWorld(t)
			page, err := query.NewListUsersPagedHandler(mems, persons).Handle(t.Context(),
				query.ListUsersPagedQuery{TenantID: testTenantID, PageSize: 10, Status: c.status})
			if err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, v := range page.Items {
				for k, id := range ids {
					if id.String() == v.MembershipID {
						got = append(got, k)
					}
				}
			}
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Errorf("listed (-want +got): %s", diff)
			}
		})
	}
}

func TestListUsersPaged_InactivePagesByCursor(t *testing.T) {
	t.Parallel()
	mems, persons, _ := statusWorld(t)
	h := query.NewListUsersPagedHandler(mems, persons)
	p1, err := h.Handle(t.Context(), query.ListUsersPagedQuery{TenantID: testTenantID, PageSize: 1, Status: membership.FilterAll})
	if err != nil || len(p1.Items) != 1 || !p1.HasMore {
		t.Fatalf("page 1 = %+v, %v; want one item and more", p1, err)
	}
	cur, err := pagination.Decode(p1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := h.Handle(t.Context(), query.ListUsersPagedQuery{TenantID: testTenantID, PageSize: 5, Status: membership.FilterAll, Cursor: cur})
	if err != nil || len(p2.Items) != 2 {
		t.Fatalf("page 2 = %+v, %v; want the other two", p2, err)
	}
}

func TestCountUsersByStatus(t *testing.T) {
	t.Parallel()
	mems, _, _ := statusWorld(t)
	got, err := query.NewCountUsersByStatusHandler(mems).Handle(t.Context(), query.CountUsersByStatusQuery{TenantID: testTenantID})
	if err != nil {
		t.Fatal(err)
	}
	if want := (query.UserStatusCounts{Active: 2, Inactive: 1}); got != want {
		t.Errorf("counts = %+v, want %+v (another tenant's member must not count)", got, want)
	}
	if _, err := query.NewCountUsersByStatusHandler(mems).Handle(t.Context(), query.CountUsersByStatusQuery{}); err == nil {
		t.Error("a zero tenant was accepted")
	}
}
