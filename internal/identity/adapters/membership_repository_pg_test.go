//go:build integration

package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// seedTenant + seedPerson are convenience helpers — repos are wired the
// same way the production composition root will wire them.
func seedTenant(t *testing.T, repo *adapters.TenantRepository) *tenant.Tenant {
	t.Helper()
	id := tenant.ID(ids.NewV7().String())
	// UUIDv7's leading chars are timestamp-derived → tests called in
	// rapid succession would collide on a 8-char prefix slug. Use the
	// trailing random portion.
	full := ids.NewV7().String()
	s, err := slug.New("ten-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("admin@example.test")
	tn, err := tenant.New(id, s, "Acme Pharma Pvt Ltd", "Acme", addr)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := repo.Add(context.Background(), tn); err != nil {
		t.Fatalf("seedTenant Add: %v", err)
	}
	return tn
}

func seedPerson(t *testing.T, repo *adapters.PersonRepository, addr string) *person.Person {
	t.Helper()
	p := newPerson(t, addr)
	if err := repo.Add(context.Background(), p); err != nil {
		t.Fatalf("seedPerson: %v", err)
	}
	return p
}

func TestMembershipRepository_Add_PersistsRowAndOutboxEvent(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "member@example.test")

	id := membership.ID(ids.NewV7().String())
	m, err := membership.New(id, p.ID(), tn.ID())
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}

	// Caller binds tenant on ctx — under TxScopeTenant, the INSERT WITH
	// CHECK passes because tenant_id = app.current_tenant().
	ctx := tenancy.WithID(context.Background(), tenancy.ID(tn.ID().String()))

	if err := memberships.Add(ctx, m); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := memberships.GetByID(ctx, m.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PersonID() != p.ID() {
		t.Fatalf("personID round-trip: got %q want %q", got.PersonID(), p.ID())
	}
	if got.TenantID() != tn.ID() {
		t.Fatalf("tenantID round-trip: got %q want %q", got.TenantID(), tn.ID())
	}
	if got.Status() != membership.StatusActive {
		t.Fatalf("status: got %v want active", got.Status())
	}
}

func TestMembershipRepository_Add_SecondActive_ReturnsErrAlreadyActive(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "switch@example.test")

	// First Active Membership in tenant A.
	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID())
	ctxA := tenancy.WithID(context.Background(), tenancy.ID(tnA.ID().String()))
	if err := memberships.Add(ctxA, mA); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Second concurrent Active Membership in tenant B — partial unique
	// index uq_memberships_person_active blocks it.
	mB, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnB.ID())
	ctxB := tenancy.WithID(context.Background(), tenancy.ID(tnB.ID().String()))
	err := memberships.Add(ctxB, mB)
	if !errors.Is(err, membership.ErrAlreadyActive) {
		t.Fatalf("expected ErrAlreadyActive, got %v", err)
	}
}

func TestMembershipRepository_GetByID_OutsideTenantScope_NotFound(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "isolation@example.test")

	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID())
	ctxA := tenancy.WithID(context.Background(), tenancy.ID(tnA.ID().String()))
	if err := memberships.Add(ctxA, mA); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Look up under tenant B's scope — RLS hides the row.
	ctxB := tenancy.WithID(context.Background(), tenancy.ID(tnB.ID().String()))
	_, err := memberships.GetByID(ctxB, mA.ID())
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("expected ErrNotFound (RLS isolation), got %v", err)
	}
}

func TestMembershipRepository_UpdateByID_DeactivateClearsActiveSlot(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "rotate@example.test")

	// Active in tenant A.
	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID())
	ctxA := tenancy.WithID(context.Background(), tenancy.ID(tnA.ID().String()))
	if err := memberships.Add(ctxA, mA); err != nil {
		t.Fatalf("Add A: %v", err)
	}

	// Deactivate in A.
	err := memberships.UpdateByID(ctxA, mA.ID(), func(m *membership.Membership) (bool, error) {
		if err := m.Deactivate("job change"); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	// Now adding an Active in B is allowed (single-Active slot freed).
	mB, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnB.ID())
	ctxB := tenancy.WithID(context.Background(), tenancy.ID(tnB.ID().String()))
	if err := memberships.Add(ctxB, mB); err != nil {
		t.Fatalf("Add B after deactivate: %v", err)
	}
}

func TestMembershipRepository_GetActiveForPerson_BypassesRLS(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "login@example.test")

	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn.ID())
	ctx := tenancy.WithID(context.Background(), tenancy.ID(tn.ID().String()))
	if err := memberships.Add(ctx, mA); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Login flow: ctx has NO tenant set yet — login resolves it via this
	// query under platform scope.
	got, err := memberships.GetActiveForPerson(context.Background(), p.ID())
	if err != nil {
		t.Fatalf("GetActiveForPerson: %v", err)
	}
	if got.ID() != mA.ID() {
		t.Fatalf("id round-trip: got %q want %q", got.ID(), mA.ID())
	}
}

func TestMembershipRepository_GetActiveForPerson_NoActive_NotFound(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	p := seedPerson(t, persons, "noactive@example.test")
	_, err := memberships.GetActiveForPerson(context.Background(), p.ID())
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

