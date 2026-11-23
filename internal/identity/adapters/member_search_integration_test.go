//go:build integration

// arch-test:parallel-safe — fresh tenants and persons on the shared pgtest
// container, RLS isolation; every name carries a per-test marker.
// arch-test:no-timeout-needed — each test is a handful of single-statement queries
// on t.Context() (bounded by the test run's own deadline).

// Identity fix round 2, M9: the member picker offers only people who can be
// assigned a lead. The SQL filters (active person, not anonymised, not globally
// suspended, active membership, this tenant) were pinned only by end-to-end
// tests that plain `go test ./...` never runs.

package adapters_test

import (
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestMemberSearchPG_OffersOnlyPeopleWhoCanBeAssigned(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons, members := adapters.NewPersonRepository(pool, tx), adapters.NewMembershipRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)
	search := adapters.NewMemberSearchPG(pool, tx)
	tnA, tnB := seedTenant(t, tenants), seedTenant(t, tenants)
	marker := "Zq" + ids.NewV7().String()[24:32] // unique to this test; letters and digits only

	// seed adds a person named "<marker> <last>" with an ACTIVE membership of tn.
	seed := func(tn tenant.ID, last string, mutate func(*person.Snapshot)) membership.ID {
		t.Helper()
		addr, err := email.New("ms-" + ids.NewV7().String() + "@example.test")
		if err != nil {
			t.Fatal(err)
		}
		base := newPerson(t, addr.String())
		snap := person.Snapshot{
			ID: base.ID(), Email: base.Email(), FirstName: marker, LastName: last, PasswordHash: base.PasswordHash(),
			SecurityStamp: base.SecurityStamp(), IsActive: true, CreatedAt: testNow,
		}
		if mutate != nil {
			mutate(&snap)
		}
		p := person.UnmarshalFromDB(snap)
		if err := persons.Add(t.Context(), p); err != nil {
			t.Fatalf("persons.Add: %v", err)
		}
		m, err := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn, membership.ID(""), testNow)
		if err != nil {
			t.Fatal(err)
		}
		if err := members.Add(tenancy.WithID(t.Context(), tenancy.ID(tn.String())), m); err != nil {
			t.Fatalf("members.Add: %v", err)
		}
		return m.ID()
	}

	want := seed(tnA.ID(), "Active", nil)
	seed(tnA.ID(), "Inactive", func(s *person.Snapshot) { s.IsActive = false })
	seed(tnA.ID(), "Anonymised", func(s *person.Snapshot) { s.IsAnonymised = true; s.AnonymisedAt = testNow })
	seed(tnA.ID(), "Suspended", func(s *person.Snapshot) {
		s.IsGloballySuspended = true
		s.GloballySuspendedAt = testNow
		s.GlobalSuspensionReason = "abuse"
	})
	seed(tnB.ID(), "OtherTenant", nil)

	ctx := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	hits, err := search.SearchActiveMembers(ctx, tnA.ID(), marker, 20)
	if err != nil {
		t.Fatalf("SearchActiveMembers: %v", err)
	}
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.DisplayName)
	}
	if len(hits) != 1 || hits[0].MembershipID != want.String() {
		t.Fatalf("hits = %v, want only the active person of this tenant (%s)", got, want)
	}

	// The tenant B search sees only its own member, never tenant A's.
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	hitsB, err := search.SearchActiveMembers(ctxB, tnB.ID(), marker, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hitsB) != 1 || !slices.Equal([]string{hitsB[0].DisplayName}, []string{marker + " OtherTenant"}) {
		t.Fatalf("tenant B hits = %v, want only its own member", hitsB)
	}
}

// A typed % or _ matches itself, not "anything".
func TestMemberSearchPG_WildcardsMatchThemselves(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons, members := adapters.NewPersonRepository(pool, tx), adapters.NewMembershipRepository(pool, tx)
	tn := seedTenant(t, adapters.NewTenantRepository(pool, tx))
	search := adapters.NewMemberSearchPG(pool, tx)
	marker := "Wq" + ids.NewV7().String()[24:32]
	for _, last := range []string{"plain", "50%off", "a_b"} {
		p := seedPerson(t, persons, "wild-"+ids.NewV7().String()+"@example.test")
		if err := persons.UpdateByID(t.Context(), p.ID(), func(x *person.Person) (bool, error) {
			return true, x.UpdateProfile(marker, last, testNow)
		}); err != nil {
			t.Fatalf("UpdateProfile: %v", err)
		}
		m, err := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn.ID(), membership.ID(""), testNow)
		if err != nil {
			t.Fatal(err)
		}
		if err := members.Add(tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String())), m); err != nil {
			t.Fatal(err)
		}
	}
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	for q, want := range map[string]int{marker + " 50%": 1, marker + " a_": 1, marker + " ": 3, "%" + marker[:3] + "%": 0} {
		hits, err := search.SearchActiveMembers(ctx, tn.ID(), q, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != want {
			t.Errorf("query %q: %d hits, want %d", q, len(hits), want)
		}
	}
}
