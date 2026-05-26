package query_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/redis/go-redis/v9"

	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// newHybridCacheForTest wires a miniredis-backed HybridCache for the
// cached-capabilities handler tests.
func newHybridCacheForTest(t *testing.T) *cache.HybridCache {
	t.Helper()
	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         cli,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)
	return hc
}

func TestNewGetCapabilitiesHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	persons := persontest.NewFakeRepository()
	mems := membershiptest.NewFakeRepository()
	roles := roletest.NewFakeRepository()

	cases := []struct {
		name string
		fn   func()
	}{
		{"nil persons", func() {
			_ = query.NewGetCapabilitiesHandler(nil, mems, roles) // arch-test:ignore-err - test fixture setup
		}},
		{"nil memberships", func() {
			_ = query.NewGetCapabilitiesHandler(persons, nil, roles) // arch-test:ignore-err - test fixture setup
		}},
		{"nil roles", func() {
			_ = query.NewGetCapabilitiesHandler(persons, mems, nil) // arch-test:ignore-err - test fixture setup
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic on nil dep")
				}
			}()
			c.fn()
		})
	}
}

func TestGetCapabilities_RejectsZeroInputs(t *testing.T) {
	t.Parallel()
	h := query.NewGetCapabilitiesHandler(
		persontest.NewFakeRepository(),
		membershiptest.NewFakeRepository(),
		roletest.NewFakeRepository(),
	)
	cases := []struct {
		name string
		q    query.GetCapabilitiesQuery
	}{
		{"zero person", query.GetCapabilitiesQuery{MembershipID: testMembershipID, TenantID: testTenantID}},
		{"zero membership", query.GetCapabilitiesQuery{PersonID: testPersonID, TenantID: testTenantID}},
		{"zero tenant", query.GetCapabilitiesQuery{PersonID: testPersonID, MembershipID: testMembershipID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(t.Context(), c.q)
			if err == nil {
				t.Fatal("expected error on missing input")
			}
		})
	}
}

func TestGetCapabilities_PropagatesPersonNotFound(t *testing.T) {
	t.Parallel()
	persons := persontest.NewFakeRepository()
	mems := membershiptest.NewFakeRepository()
	h := query.NewGetCapabilitiesHandler(persons, mems, roletest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetCapabilitiesQuery{
		PersonID:     testPersonID,
		MembershipID: testMembershipID,
		TenantID:     testTenantID,
	})
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

func TestGetCapabilities_PropagatesMembershipNotFound(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	mems := membershiptest.NewFakeRepository()
	h := query.NewGetCapabilitiesHandler(persons, mems, roletest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetCapabilitiesQuery{
		PersonID:     p.ID(),
		MembershipID: testMembershipID,
		TenantID:     testTenantID,
	})
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("err = %v, want membership.ErrNotFound", err)
	}
}

func TestGetCapabilities_NoRolesReturnsEmptyRolesSlice(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	m := newMembership(t, testMembershipID, p.ID(), testTenantID)
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetCapabilitiesHandler(persons, mems, roletest.NewFakeRepository())
	view, err := h.Handle(t.Context(), query.GetCapabilitiesQuery{
		PersonID:     p.ID(),
		MembershipID: m.ID(),
		TenantID:     testTenantID,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if view.Email != testEmail {
		t.Errorf("Email = %q, want %q", view.Email, testEmail)
	}
	if view.FirstName != "Alice" || view.LastName != "Liddell" {
		t.Errorf("Name = %q %q", view.FirstName, view.LastName)
	}
	if view.Roles != nil {
		t.Errorf("Roles = %v, want nil (no role assignments)", view.Roles)
	}
}

func TestGetCapabilities_HappyPath_ProjectsAllFields(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	m := newMembership(t, testMembershipID, p.ID(), testTenantID)
	r := newRole(t, testRoleID, testTenantID, "Admin")
	sa := newSuperAdminRole(t, role.ID("99999999-9999-9999-9999-999999999999"), testTenantID)
	if err := m.AssignRole(r.ID(), testNow); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := m.AssignRole(sa.ID(), testNow); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
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
	if err := roles.Add(t.Context(), sa); err != nil {
		t.Fatal(err)
	}

	h := query.NewGetCapabilitiesHandler(persons, mems, roles)
	view, err := h.Handle(t.Context(), query.GetCapabilitiesQuery{
		PersonID:      p.ID(),
		MembershipID:  m.ID(),
		TenantID:      testTenantID,
		SecurityStamp: "stamp-1",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if view.Email != testEmail || view.FirstName != "Alice" || view.LastName != "Liddell" {
		t.Errorf("profile = %+v", view)
	}
	if len(view.Roles) != 2 {
		t.Fatalf("Roles len = %d, want 2", len(view.Roles))
	}
	// Order is unspecified by the SQL adapter contract; assert by name.
	gotSA := false
	gotAdmin := false
	for _, rv := range view.Roles {
		switch rv.Name {
		case "SuperAdmin":
			gotSA = true
			if !rv.IsSuperAdmin {
				t.Errorf("SuperAdmin role IsSuperAdmin=false")
			}
		case "Admin":
			gotAdmin = true
			if rv.IsSuperAdmin {
				t.Errorf("Admin role IsSuperAdmin=true")
			}
		}
	}
	if !gotSA || !gotAdmin {
		t.Errorf("missing roles: gotSA=%v gotAdmin=%v", gotSA, gotAdmin)
	}
}

// rolesErrRepo wraps the fake to surface a controlled error on
// GetByIDs — covers the `roles.GetByIDs` error branch of the handler.
type rolesErrRepo struct {
	role.Repository
	err error
}

func (r rolesErrRepo) GetByIDs(_ context.Context, _ tenant.ID, _ []role.ID) ([]*role.Role, error) {
	return nil, r.err
}

func TestGetCapabilities_PropagatesRolesGetByIDsError(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	m := newMembership(t, testMembershipID, p.ID(), testTenantID)
	if err := m.AssignRole(testRoleID, testNow); err != nil {
		t.Fatal(err)
	}
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("roles boom")
	roles := rolesErrRepo{Repository: roletest.NewFakeRepository(), err: sentinel}

	h := query.NewGetCapabilitiesHandler(persons, mems, roles)
	_, err := h.Handle(t.Context(), query.GetCapabilitiesQuery{
		PersonID:     p.ID(),
		MembershipID: m.ID(),
		TenantID:     testTenantID,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// ----- CachedGetCapabilitiesHandler ----------------------------------------

func TestNewCachedGetCapabilitiesHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	hc := newHybridCacheForTest(t)
	inner := query.NewGetCapabilitiesHandler(
		persontest.NewFakeRepository(),
		membershiptest.NewFakeRepository(),
		roletest.NewFakeRepository(),
	)
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil cache", func() {
			_ = query.NewCachedGetCapabilitiesHandler(inner, nil, membershiptest.NewFakeRepository()) // arch-test:ignore-err - test fixture setup
		}},
		{"nil memberships", func() {
			_ = query.NewCachedGetCapabilitiesHandler(inner, hc, nil) // arch-test:ignore-err - test fixture setup
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

func TestCachedGetCapabilities_FactoryHydratesAndCachesView(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	m := newMembership(t, testMembershipID, p.ID(), testTenantID)
	persons := persontest.NewFakeRepository()
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	mems := membershiptest.NewFakeRepository()
	if err := mems.Add(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	roles := roletest.NewFakeRepository()
	inner := query.NewGetCapabilitiesHandler(persons, mems, roles)

	hc := newHybridCacheForTest(t)
	h := query.NewCachedGetCapabilitiesHandler(inner, hc, mems)

	q := query.GetCapabilitiesQuery{
		PersonID:      p.ID(),
		MembershipID:  m.ID(),
		TenantID:      testTenantID,
		SecurityStamp: "stamp-A",
	}
	got, err := h.Handle(t.Context(), q)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if diff := cmp.Diff(query.CapabilitiesView{
		Email:     testEmail,
		FirstName: "Alice",
		LastName:  "Liddell",
		Roles:     nil,
	}, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("view (-want +got)\n%s", diff)
	}

	// Second call returns the same view from cache (the factory would
	// rebuild it identically anyway — but exercising the cache path is
	// the point).
	hc.L1.Wait()
	got2, err := h.Handle(t.Context(), q)
	if err != nil {
		t.Fatalf("Handle 2: %v", err)
	}
	if diff := cmp.Diff(got, got2); diff != "" {
		t.Errorf("cached view drift (-first +second)\n%s", diff)
	}
}

func TestCachedGetCapabilities_FactoryMembershipNotFound_Surfaces(t *testing.T) {
	t.Parallel()
	persons := persontest.NewFakeRepository()
	mems := membershiptest.NewFakeRepository()
	roles := roletest.NewFakeRepository()
	inner := query.NewGetCapabilitiesHandler(persons, mems, roles)

	hc := newHybridCacheForTest(t)
	h := query.NewCachedGetCapabilitiesHandler(inner, hc, mems)
	_, err := h.Handle(t.Context(), query.GetCapabilitiesQuery{
		PersonID:      testPersonID,
		MembershipID:  testMembershipID,
		TenantID:      testTenantID,
		SecurityStamp: "stamp-A",
	})
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("err = %v, want membership.ErrNotFound", err)
	}
}
