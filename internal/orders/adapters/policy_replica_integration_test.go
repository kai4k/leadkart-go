//go:build integration

// SQL-CONTRACT COVERAGE for this file (ADR 0062): the version-guarded upsert and
// the read of orders.policy_replica against real Postgres under RLS.
//
// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` bound execution.
//
// arch-test:no-synctest — the deadlock guard waits on a real pgx pool against a real
// Postgres; virtual time cannot model a connection wait.
//
// arch-test:parallel-safe — every test mints a FRESH tenant_id and the table is
// RLS-scoped, so tests are isolated by tenant.

package adapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters"
)

func policyAt(months int) commercepolicy.Policy {
	p := commercepolicy.Default()
	p.Expiry.StopMonthsBefore = months
	return p
}

func newReplica(t *testing.T) (*adapters.PolicyReplicaRepository, *pg.Transactor) {
	t.Helper()
	p := ordersPool(t)
	tx := pg.NewTransactor(p)
	return adapters.NewPolicyReplicaRepository(p, tx), tx
}

func TestPolicyReplica_NoRowReadsInitial(t *testing.T) {
	t.Parallel()
	repo, _ := newReplica(t)
	got, err := repo.ReadPolicy(t.Context(), tenant.ID(ids.NewV7().String()))
	if err != nil {
		t.Fatalf("ReadPolicy: %v", err)
	}
	if diff := cmp.Diff(commercepolicy.Initial(), got); diff != "" {
		t.Fatalf("a tenant with no replica row must read commercepolicy.Initial() (-want +got):\n%s", diff)
	}
}

func TestPolicyReplica_StoresOnlyNewerVersions(t *testing.T) {
	t.Parallel()
	repo, _ := newReplica(t)
	tid := tenant.ID(ids.NewV7().String())
	store := func(version int64, months int) bool {
		t.Helper()
		ok, err := repo.StoreIfNewer(t.Context(), tid, commercepolicy.Versioned{Version: version, Policy: policyAt(months)})
		if err != nil {
			t.Fatalf("StoreIfNewer v%d: %v", version, err)
		}
		return ok
	}
	read := func() commercepolicy.Versioned {
		t.Helper()
		v, err := repo.ReadPolicy(t.Context(), tid)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	if !store(2, 3) {
		t.Fatal("the first version must be stored")
	}
	if store(2, 9) {
		t.Fatal("the same version again (a replay) must store nothing")
	}
	if v := read(); v.Version != 2 || v.Policy.Expiry.StopMonthsBefore != 3 {
		t.Fatalf("a replay changed the replica: %+v", v)
	}
	if store(1, 7) {
		t.Fatal("an older version arriving late must store nothing")
	}
	if v := read(); v.Version != 2 {
		t.Fatalf("an older event rolled the replica back to v%d", v.Version)
	}
	if !store(5, 6) {
		t.Fatal("a newer version must be stored (a skipped version is fine: the policy is whole)")
	}
	if v := read(); v.Version != 5 || v.Policy.Expiry.StopMonthsBefore != 6 {
		t.Fatalf("replica = %+v", v)
	}
}

func TestPolicyReplica_RowLevelSecurityIsolatesTenants(t *testing.T) {
	t.Parallel()
	repo, tx := newReplica(t)
	a, b := tenant.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String())
	if _, err := repo.StoreIfNewer(t.Context(), a, commercepolicy.Versioned{Version: 1, Policy: policyAt(5)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.ReadPolicy(t.Context(), b); got.Version != 0 {
		t.Fatalf("tenant B read tenant A's replica: %+v", got)
	}
	// Under tenant B's session, a write aimed at tenant A's row is refused by the
	// policy WITH CHECK, not silently applied.
	err := tx.WithinTenantTx(t.Context(), b.String(), func(ctx context.Context) error {
		_, serr := repo.StoreIfNewer(ctx, a, commercepolicy.Versioned{Version: 9, Policy: policyAt(1)})
		return serr
	})
	if err == nil {
		t.Fatal("tenant B wrote tenant A's replica row")
	}
	if got, _ := repo.ReadPolicy(t.Context(), a); got.Version != 1 || got.Policy.Expiry.StopMonthsBefore != 5 {
		t.Fatalf("tenant A's replica was touched: %+v", got)
	}
}

// The replica is read THROUGH the transaction already open on ctx: with a pool
// of ONE connection a second pooled connection would deadlock (review finding
// C1's shape), and the read sees the caller's uncommitted write.
func TestPolicyReplica_ReadsInsideTheCallersTransactionWithoutASecondConnection(t *testing.T) {
	t.Parallel()
	cfg, err := pgxpool.ParseConfig(sharedPG.AppDSN())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("one-connection pool: %v", err)
	}
	t.Cleanup(one.Close)
	tx := pg.NewTransactor(one)
	repo := adapters.NewPolicyReplicaRepository(one, tx)
	tid := tenant.ID(ids.NewV7().String())

	done := make(chan error, 1)
	go func() {
		done <- tx.WithinTenantTx(t.Context(), tid.String(), func(ctx context.Context) error {
			if _, err := repo.StoreIfNewer(ctx, tid, commercepolicy.Versioned{Version: 3, Policy: policyAt(8)}); err != nil {
				return err
			}
			got, err := repo.ReadPolicy(ctx, tid) // uncommitted: only visible on this very connection
			if err != nil {
				return err
			}
			if got.Version != 3 || got.Policy.Expiry.StopMonthsBefore != 8 {
				t.Errorf("the read did not see the caller's transaction: %+v", got)
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read inside the caller's transaction: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ReadPolicy took a second pooled connection while the caller held the only one (deadlock)")
	}
}
