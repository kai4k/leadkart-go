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

// membershipsErrRepo lets a test inject failures on the listing
// methods.
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

// personsErrRepo lets a test inject failure on the batch-hydration
// path.
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
	// Membership references a Person that the persons repo will NOT
	// return (race-with-soft-delete simulation).
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
	// Two memberships, two persons.
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
	// Seed one row so the page query returns IDs the hydrate path must consume.
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
	for i := 0; i < 3; i++ {
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
	for i := 0; i < 3; i++ {
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
	// Just verify that PageSize=0 doesn't crash; the clamp pads it to
	// DefaultPageSize internally.
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
	// Use distinct joinedAts so cursor predicate cuts cleanly.
	for i := 0; i < 3; i++ {
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
	// Caller-supplied cursor at the SECOND joinedAt — should return
	// only the row joined BEFORE it.
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
	// joinedAt[0] is strictly Before the cursor — admitted.
	// joinedAt[1] is Equal but the cursor ID is lexicographically
	// greater than any seeded membership ID, so admitted too.
	if got, want := len(page.Items), 2; got != want {
		t.Errorf("items = %d, want %d", got, want)
	}
}

// Ensure roletest import is exercised so go vet doesn't flag in case
// future refactors trim direct uses.
var _ = role.HierarchyLevelDefault
