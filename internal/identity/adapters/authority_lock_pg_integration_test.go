//go:build integration

// SQL-CONTRACT COVERAGE for this file (ADR 0062): the per-tenant authority
// lock is a transaction-scoped Postgres advisory lock. It must block a second
// transaction of the SAME tenant, leave other tenants alone, release at
// commit, and refuse to run outside a UnitOfWork transaction.
//
// arch-test:serial — the tests hold real locks; they run one at a time.

package adapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// SQL contract: advisory xact lock serialises same-tenant writers.
func TestAuthorityLockPG_SerialisesSameTenantWriters(t *testing.T) {
	pool := repoFixture(t)
	uow := pg.NewTransactor(pool)
	lock := adapters.NewAuthorityLockPG()
	tA := tenant.ID(ids.NewV7().String())
	tB := tenant.ID(ids.NewV7().String())

	// attempt takes tenantID's lock in a NEW transaction under a short deadline.
	attempt := func(tenantID tenant.ID) error {
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		return uow.WithinTx(tenancy.WithID(ctx, tenancy.ID(tenantID.String())), pg.TxScopeTenant, func(ctx context.Context) error {
			return lock.LockTenantAuthority(ctx, tenantID)
		})
	}

	err := uow.WithinTx(tenancy.WithID(t.Context(), tenancy.ID(tA.String())), pg.TxScopeTenant, func(ctx context.Context) error {
		if err := lock.LockTenantAuthority(ctx, tA); err != nil {
			return err
		}
		if err := attempt(tA); err == nil {
			t.Error("a second transaction took tenant A's lock while the first held it")
		}
		if err := attempt(tB); err != nil {
			t.Errorf("tenant B's lock blocked by tenant A's holder: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("holder transaction: %v", err)
	}
	if err := attempt(tA); err != nil {
		t.Errorf("tenant A's lock not released at commit: %v", err)
	}
}

func TestAuthorityLockPG_RefusesToRunOutsideATransaction(t *testing.T) {
	lock := adapters.NewAuthorityLockPG()
	if err := lock.LockTenantAuthority(t.Context(), tenant.ID(ids.NewV7().String())); err == nil {
		t.Fatal("LockTenantAuthority outside a UnitOfWork transaction returned nil: the lock would release immediately and protect nothing")
	}
}
