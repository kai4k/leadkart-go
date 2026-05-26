package query_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// ----- GetPersonHandler ----------------------------------------------------

func TestNewGetPersonHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewGetPersonHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestGetPerson_RejectsZeroID(t *testing.T) {
	t.Parallel()
	h := query.NewGetPersonHandler(persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetPersonQuery{})
	if err == nil {
		t.Fatal("expected error on zero person id")
	}
}

func TestGetPerson_NotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetPersonHandler(persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetPersonQuery{PersonID: testPersonID})
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

func TestGetPerson_HappyPath_ProjectsAllFields(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetPersonHandler(persons)
	got, err := h.Handle(t.Context(), query.GetPersonQuery{PersonID: p.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ID != p.ID().String() {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Email != testEmail {
		t.Errorf("Email = %q", got.Email)
	}
	if got.FirstName != "Alice" || got.LastName != "Liddell" {
		t.Errorf("Name = %q %q", got.FirstName, got.LastName)
	}
	if !got.IsActive {
		t.Errorf("IsActive = false")
	}
	if got.IsAnonymised || got.IsGloballySuspended {
		t.Errorf("flags wrong: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero")
	}
}

// ----- GetPersonByEmailHandler --------------------------------------------

func TestNewGetPersonByEmailHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewGetPersonByEmailHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestGetPersonByEmail_RejectsZeroEmail(t *testing.T) {
	t.Parallel()
	h := query.NewGetPersonByEmailHandler(persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetPersonByEmailQuery{})
	if err == nil {
		t.Fatal("expected error on zero email")
	}
}

func TestGetPersonByEmail_NotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetPersonByEmailHandler(persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetPersonByEmailQuery{Email: mustEmail(t, "nobody@example.test")})
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

func TestGetPersonByEmail_HappyPath(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetPersonByEmailHandler(persons)
	got, err := h.Handle(t.Context(), query.GetPersonByEmailQuery{Email: mustEmail(t, testEmail)})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ID != p.ID().String() {
		t.Errorf("ID = %q", got.ID)
	}
}

// ----- ListPersonMembershipsHandler ---------------------------------------

func TestNewListPersonMembershipsHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	mems := membershiptest.NewFakeRepository()
	persons := persontest.NewFakeRepository()
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil memberships", func() {
			_ = query.NewListPersonMembershipsHandler(nil, persons) // arch-test:ignore-err - test fixture setup
		}},
		{"nil persons", func() {
			_ = query.NewListPersonMembershipsHandler(mems, nil) // arch-test:ignore-err - test fixture setup
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

func TestListPersonMemberships_RejectsZeroPerson(t *testing.T) {
	t.Parallel()
	h := query.NewListPersonMembershipsHandler(membershiptest.NewFakeRepository(), persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListPersonMembershipsQuery{})
	if err == nil {
		t.Fatal("expected error on zero person")
	}
}

func TestListPersonMemberships_PersonNotFoundSurfacesErr(t *testing.T) {
	t.Parallel()
	h := query.NewListPersonMembershipsHandler(membershiptest.NewFakeRepository(), persontest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListPersonMembershipsQuery{PersonID: testPersonID})
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

// personsErr2Repo lets a test return a non-ErrNotFound failure on GetByID.
type personsErr2Repo struct {
	person.Repository
	err error
}

func (r personsErr2Repo) GetByID(_ context.Context, _ person.ID) (*person.Person, error) {
	return nil, r.err
}

func TestListPersonMemberships_PropagatesGenericPersonError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("person boom")
	persons := personsErr2Repo{Repository: persontest.NewFakeRepository(), err: sentinel}
	h := query.NewListPersonMembershipsHandler(membershiptest.NewFakeRepository(), persons)
	_, err := h.Handle(t.Context(), query.ListPersonMembershipsQuery{PersonID: testPersonID})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// membershipsListAllErrRepo lets a test inject failure on ListAllForPerson.
type membershipsListAllErrRepo struct {
	membership.Repository
	err error
}

func (r membershipsListAllErrRepo) ListAllForPerson(_ context.Context, _ person.ID) ([]*membership.Membership, error) {
	return nil, r.err
}

func TestListPersonMemberships_PropagatesListError(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("list-mem boom")
	mems := membershipsListAllErrRepo{Repository: membershiptest.NewFakeRepository(), err: sentinel}
	h := query.NewListPersonMembershipsHandler(mems, persons)
	_, err := h.Handle(t.Context(), query.ListPersonMembershipsQuery{PersonID: p.ID()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListPersonMemberships_HappyPath(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	m1 := newMembership(t, membership.ID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), p.ID(), testTenantID)
	m2 := newMembership(t, membership.ID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), p.ID(), tenant.ID("99999999-9999-9999-9999-999999999999"))
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m1); err != nil {
		t.Fatal(err)
	}
	// m2 is in a different tenant — single Active-per-Person invariant
	// would refuse, so deactivate m1 first to allow m2 Active. Actually:
	// the fake's Add rejects a second Active for same Person regardless
	// of tenant — keep m2 Inactive.
	if err := m2.Deactivate("seed for test", testNow); err != nil {
		t.Fatal(err)
	}
	if err := mems.Add(t.Context(), m2); err != nil {
		t.Fatal(err)
	}

	h := query.NewListPersonMembershipsHandler(mems, persons)
	got, err := h.Handle(t.Context(), query.ListPersonMembershipsQuery{PersonID: p.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, v := range got {
		if v.PersonID != p.ID().String() {
			t.Errorf("PersonID mismatch on row: %+v", v)
		}
	}
}

// ----- ListAllTenantsHandler ----------------------------------------------

func TestNewListAllTenantsHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListAllTenantsHandler(nil) // arch-test:ignore-err - test fixture setup
}

// tenantsErrRepo injects a failure on ListAll.
type tenantsErrRepo struct {
	tenant.Repository
	err error
}

func (r tenantsErrRepo) ListAll(_ context.Context) ([]*tenant.Tenant, error) {
	return nil, r.err
}

func TestListAllTenants_PropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list-all boom")
	repo := tenantsErrRepo{Repository: tenanttest.NewFakeRepository(), err: sentinel}
	h := query.NewListAllTenantsHandler(repo)
	_, err := h.Handle(t.Context(), query.ListAllTenantsQuery{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListAllTenants_HappyPath(t *testing.T) {
	t.Parallel()
	repo := tenanttest.NewFakeRepository()
	tn1 := newTenant(t, testTenantID, testTenantSlugStr)
	tn2 := newTenant(t, tenant.ID("99999999-9999-9999-9999-999999999999"), "beta-pharma")
	if err := repo.Add(t.Context(), tn1); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(t.Context(), tn2); err != nil {
		t.Fatal(err)
	}
	h := query.NewListAllTenantsHandler(repo)
	got, err := h.Handle(t.Context(), query.ListAllTenantsQuery{})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestListAllTenants_EmptyRepoReturnsEmpty(t *testing.T) {
	t.Parallel()
	h := query.NewListAllTenantsHandler(tenanttest.NewFakeRepository())
	got, err := h.Handle(t.Context(), query.ListAllTenantsQuery{})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
