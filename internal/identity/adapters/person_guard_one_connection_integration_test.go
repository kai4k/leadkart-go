//go:build integration

// arch-test:parallel-safe — own small pools on the shared pgtest container, fresh
// tenant_ids and persons, RLS isolation.
//
// arch-test:raw-sql-justified — the forced-error test runs `SELECT 1/0` inside the
// savepoint (no repository method fails on demand) and reads the app.is_platform
// setting directly: it is the setting, not a row, that the test is about.

// Identity fix round 2, I2: the person change guard (global suspension)
// holds the tenant authority lock and the person row while it
// re-reads the person's memberships. That read used to take a SECOND pooled
// connection; one guard run plus pool-1 requests waiting on its locks held every
// connection of the pool until the callers gave up, for every tenant. The read
// now runs on the guard's own transaction.

package adapters_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// guardWorld is two tenants seeded through the shared pool: tenant A with its
// members, and tenant B with one member of its own (a row another tenant must
// never see).
type guardWorld struct {
	a, b    *tenant.Tenant
	inB     *membership.Membership
	seedTx  *pg.Transactor
	persons *adapters.PersonRepository
	members *adapters.MembershipRepository
	roles   *adapters.RoleRepository
}

func newGuardWorld(t *testing.T) *guardWorld {
	t.Helper()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	w := &guardWorld{seedTx: tx, persons: adapters.NewPersonRepository(pool, tx),
		members: adapters.NewMembershipRepository(pool, tx), roles: adapters.NewRoleRepository(pool, tx)}
	tenants := adapters.NewTenantRepository(pool, tx)
	w.a, w.b = seedTenant(t, tenants), seedTenant(t, tenants)
	_, w.inB = w.member(t, w.b.ID())
	return w
}

// member adds a new person with an ACTIVE membership of the tenant, holding roles.
func (w *guardWorld) member(t *testing.T, tid tenant.ID, roles ...role.ID) (*person.Person, *membership.Membership) {
	t.Helper()
	p := seedPerson(t, w.persons, "guard-"+ids.NewV7().String()+"@example.test")
	m, err := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tid, membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	for _, r := range roles {
		if err := m.AssignRole(r, testNow); err != nil {
			t.Fatalf("AssignRole: %v", err)
		}
	}
	if err := w.members.Add(tenancy.WithID(t.Context(), tenancy.ID(tid.String())), m); err != nil {
		t.Fatalf("members.Add: %v", err)
	}
	return p, m
}

// ownerRole seeds the tenant's Company Owner role.
func (w *guardWorld) ownerRole(t *testing.T, tid tenant.ID) role.ID {
	t.Helper()
	r, err := role.New(role.ID(ids.NewV7().String()), tid, role.SystemRoles.Tenant.CompanyOwner, true, role.HierarchyLevelOwner, false, testNow)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := w.roles.Add(tenancy.WithID(t.Context(), tenancy.ID(tid.String())), r); err != nil {
		t.Fatalf("roles.Add: %v", err)
	}
	return r.ID()
}

// smallPool is a pool of n connections on the shared container, with the
// repositories and the guard wired over it exactly as the composition root does.
type smallPool struct {
	tx      *pg.Transactor
	members *adapters.MembershipRepository
	persons *adapters.PersonRepository
	guard   command.PersonChangeGuard
}

func newSmallPool(t *testing.T, n int32) smallPool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(sharedPG.AppDSN())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = n
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	tx := pg.NewTransactor(pool)
	members, persons := adapters.NewMembershipRepository(pool, tx), adapters.NewPersonRepository(pool, tx)
	return smallPool{tx: tx, members: members, persons: persons,
		guard: command.NewPersonChangeGuard(tx, adapters.NewAuthorityLockPG(), members, adapters.NewRoleRepository(pool, tx), persons, func() time.Time { return testNow })}
}

func inTenant(ctx context.Context, tid tenant.ID) context.Context {
	return tenancy.WithID(ctx, tenancy.ID(tid.String()))
}

// The read sees the person's active membership whatever tenant the caller's
// transaction is scoped to, uses that transaction (on a pool of ONE connection a
// read that asked for its own could never be served) and leaves the transaction
// tenant-scoped: another tenant's row is as invisible after it as before.
func TestMembershipRepository_ListActiveRefsForPersonInTx_ReadsEveryTenantOnTheCallersTxAndLeavesItTenantScoped(t *testing.T) {
	t.Parallel()
	w := newGuardWorld(t)
	p, m := w.member(t, w.a.ID())
	sp := newSmallPool(t, 1)
	ctx, cancel := context.WithTimeout(inTenant(t.Context(), w.b.ID()), 5*time.Second)
	defer cancel()

	err := sp.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if _, err := sp.members.GetByID(ctx, w.a.ID(), m.ID()); !errors.Is(err, membership.ErrNotFound) {
			t.Errorf("before: tenant A's membership read inside tenant B's transaction = %v, want ErrNotFound", err)
		}
		refs, err := sp.members.ListActiveRefsForPersonInTx(ctx, p.ID())
		if err != nil {
			return err
		}
		if len(refs) != 1 || refs[0].ID != m.ID() || refs[0].TenantID != w.a.ID() {
			t.Errorf("refs = %+v, want the person's one active membership %s in tenant %s", refs, m.ID(), w.a.ID())
		}
		if _, err := sp.members.GetByID(ctx, w.a.ID(), m.ID()); !errors.Is(err, membership.ErrNotFound) {
			t.Errorf("after: tenant A's membership read inside tenant B's transaction = %v, want ErrNotFound (the transaction must be back in tenant scope)", err)
		}
		if _, err := sp.members.GetByID(ctx, w.b.ID(), w.inB.ID()); err != nil {
			t.Errorf("after: tenant B's own membership = %v, want it readable", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("on a one-connection pool: %v (the read must use the caller's transaction, not wait for a second connection)", err)
	}
}

// Only ACTIVE memberships are listed.
func TestMembershipRepository_ListActiveRefsForPersonInTx_ListsOnlyActiveMemberships(t *testing.T) {
	t.Parallel()
	w := newGuardWorld(t)
	p, m := w.member(t, w.a.ID())
	if err := w.members.UpdateByID(t.Context(), w.a.ID(), m.ID(), func(m *membership.Membership) (bool, error) {
		return true, m.Deactivate("left", testNow)
	}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	sp := newSmallPool(t, 1)
	ctx, cancel := context.WithTimeout(inTenant(t.Context(), w.a.ID()), 5*time.Second)
	defer cancel()
	err := sp.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		refs, err := sp.members.ListActiveRefsForPersonInTx(ctx, p.ID())
		if err == nil && len(refs) != 0 {
			t.Errorf("refs = %+v, want none: the person's only membership is inactive", refs)
		}
		return err
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
}

// Without a transaction on the context the read refuses at once: it never opens
// a connection of its own (here the pool's only connection is held).
func TestMembershipRepository_ListActiveRefsForPersonInTx_RefusesWithoutATransaction(t *testing.T) {
	t.Parallel()
	w := newGuardWorld(t)
	p, _ := w.member(t, w.a.ID())
	sp := newSmallPool(t, 1)
	outer, cancel := context.WithTimeout(inTenant(t.Context(), w.a.ID()), 5*time.Second)
	defer cancel()
	err := sp.tx.WithinTx(outer, pg.TxScopeTenant, func(context.Context) error {
		began := time.Now()
		_, err := sp.members.ListActiveRefsForPersonInTx(outer, p.ID()) // outer carries no transaction
		if !errors.Is(err, adapters.ErrNoTx) {
			t.Errorf("without a transaction: %v, want ErrNoTx", err)
		}
		if waited := time.Since(began); waited > time.Second {
			t.Errorf("the refusal took %s: it waited for a connection", waited)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("holding transaction: %v", err)
	}
}

// A read that fails inside the savepoint leaves the transaction usable and
// tenant-scoped: the rollback to the savepoint restores the setting on the error
// path too.
func TestMembershipRepository_PlatformReadOnTx_AFailedReadRestoresTenantScope(t *testing.T) {
	t.Parallel()
	w := newGuardWorld(t)
	_, m := w.member(t, w.a.ID())
	sp := newSmallPool(t, 1)
	ctx, cancel := context.WithTimeout(inTenant(t.Context(), w.b.ID()), 5*time.Second)
	defer cancel()

	isPlatform := func(ctx context.Context, q pgx.Tx) string {
		var v *string
		if err := q.QueryRow(ctx, `SELECT current_setting($1, true)`, pg.GUCIsPlatform).Scan(&v); err != nil {
			t.Fatalf("read %s: %v", pg.GUCIsPlatform, err)
		}
		if v == nil {
			return ""
		}
		return *v
	}
	err := sp.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		tx, _ := pg.TxFromContext(ctx)
		inside := ""
		failed := adapters.ReadAsPlatformOnTxForTest(ctx, tx, func(q pgx.Tx) error {
			inside = isPlatform(ctx, q)
			_, err := q.Exec(ctx, `SELECT 1/0`)
			return err
		})
		if failed == nil {
			t.Error("the failing read reported no error")
		}
		if inside != "true" {
			t.Errorf("inside the savepoint %s = %q, want true (the read is platform-wide)", pg.GUCIsPlatform, inside)
		}
		if got := isPlatform(ctx, tx); got == "true" {
			t.Errorf("after the failed read %s is still %q on the transaction", pg.GUCIsPlatform, got)
		}
		if _, err := sp.members.GetByID(ctx, w.a.ID(), m.ID()); !errors.Is(err, membership.ErrNotFound) {
			t.Errorf("after the failed read: tenant A's membership inside tenant B's transaction = %v, want ErrNotFound (usable, and tenant-scoped)", err)
		}
		if _, err := sp.members.GetByID(ctx, w.b.ID(), w.inB.ID()); err != nil {
			t.Errorf("after the failed read: tenant B's own membership = %v, want it readable", err)
		}

		// The caller's context is cancelled while the read runs: the scope is still
		// taken off the transaction (the rollback does not ride the cancelled context).
		gone, cancelGone := context.WithCancel(ctx)
		defer cancelGone()
		if err := adapters.ReadAsPlatformOnTxForTest(gone, tx, func(pgx.Tx) error {
			cancelGone()
			return gone.Err()
		}); !errors.Is(err, context.Canceled) {
			t.Errorf("a read whose caller went away = %v, want context.Canceled", err)
		}
		if got := isPlatform(ctx, tx); got == "true" {
			t.Errorf("after a cancelled read %s is still %q on the transaction", pg.GUCIsPlatform, got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
}

// When the savepoint cannot be rolled back the transaction may still carry the
// platform scope, so the read reports an error even though it read: the caller
// must not carry on in that transaction. Here the read releases the savepoint
// itself, which keeps the setting.
func TestMembershipRepository_PlatformReadOnTx_AScopeThatCannotBeRestoredIsAnError(t *testing.T) {
	t.Parallel()
	w := newGuardWorld(t)
	sp := newSmallPool(t, 1)
	ctx, cancel := context.WithTimeout(inTenant(t.Context(), w.b.ID()), 5*time.Second)
	defer cancel()
	abandon := errors.New("the test abandons this transaction")
	err := sp.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		tx, _ := pg.TxFromContext(ctx)
		got := adapters.ReadAsPlatformOnTxForTest(ctx, tx, func(q pgx.Tx) error { return q.Commit(ctx) }) // RELEASE SAVEPOINT
		if got == nil {
			t.Error("the read reported success although its savepoint could not be rolled back: the caller would go on with the platform scope")
		}
		return abandon
	})
	if !errors.Is(err, abandon) {
		t.Fatalf("transaction: %v", err)
	}
}

// Inside a platform transaction (the guard's path for a person with no active
// membership) the read leaves the transaction platform-scoped, as it found it.
func TestMembershipRepository_ListActiveRefsForPersonInTx_InsideAPlatformTransactionKeepsItPlatform(t *testing.T) {
	t.Parallel()
	w := newGuardWorld(t)
	p, m := w.member(t, w.a.ID())
	sp := newSmallPool(t, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := sp.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		refs, err := sp.members.ListActiveRefsForPersonInTx(ctx, p.ID())
		if err != nil {
			return err
		}
		if len(refs) != 1 || refs[0].ID != m.ID() {
			t.Errorf("refs = %+v, want %s", refs, m.ID())
		}
		if _, err := sp.members.GetByID(ctx, w.a.ID(), m.ID()); err != nil {
			t.Errorf("after the read the platform transaction lost its scope: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("platform transaction on a one-connection pool: %v", err)
	}
}

// One guard run on a pool of ONE connection: every read it makes while it holds
// the tenant authority lock and the person row goes through its transaction.
// Covers an owner (the owner set is loaded), an ordinary member, and a person
// with no membership (the guard's platform transaction).
func TestPersonChangeGuard_OnAOneConnectionPool_NeverWaitsForASecondConnection(t *testing.T) {
	t.Parallel()
	w := newGuardWorld(t)
	ownerRole := w.ownerRole(t, w.a.ID())
	owner, _ := w.member(t, w.a.ID(), ownerRole)
	w.member(t, w.a.ID(), ownerRole) // a second active owner, so the first may be suspended
	plain, _ := w.member(t, w.a.ID())
	lone := seedPerson(t, w.persons, "guard-lone-"+ids.NewV7().String()+"@example.test")
	sp := newSmallPool(t, 1)
	suspend := command.NewGlobalSuspendPersonHandler(sp.guard, sp.persons, func() time.Time { return testNow })

	for name, id := range map[string]person.ID{"an owner": owner.ID(), "a member": plain.ID(), "a person with no membership": lone.ID()} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		began := time.Now()
		err := suspend.Handle(ctx, command.GlobalSuspendPersonCommand{PersonID: id, Reason: "pool of one"})
		cancel()
		if err != nil {
			t.Fatalf("%s: suspend on a one-connection pool = %v after %s (the guard must never wait for a second connection)", name, err, time.Since(began))
		}
		got, err := w.persons.GetByID(t.Context(), id)
		if err != nil || !got.IsGloballySuspended() {
			t.Fatalf("%s: suspended=%v err=%v, want the suspension applied", name, got != nil && got.IsGloballySuspended(), err)
		}
	}
}

// Pool saturation: a pool of 4 and 12 guard runs at once in ONE tenant. They
// queue on that tenant's authority lock, each holding its connection, so the run
// that holds the lock finds no free connection: before the fix it waited for one
// that was never released, and every other request of every tenant queued behind
// it. All 12 must succeed and an unrelated read of another tenant must answer.
func TestPersonChangeGuard_AtPoolSaturation_NeverWaitsForASecondConnection(t *testing.T) {
	t.Parallel()
	const (
		poolSize = 4
		runs     = 12
		deadline = 8 * time.Second
	)
	w := newGuardWorld(t)
	people := make([]*person.Person, runs)
	for i := range people {
		people[i], _ = w.member(t, w.a.ID())
	}
	sp := newSmallPool(t, poolSize)
	suspend := command.NewGlobalSuspendPersonHandler(sp.guard, sp.persons, func() time.Time { return testNow })
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		errs    = make([]error, runs)
		readErr error
		readDur time.Duration
	)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = suspend.Handle(ctx, command.GlobalSuspendPersonCommand{PersonID: people[i].ID(), Reason: "pool saturation"})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		began := time.Now()
		_, readErr = sp.members.GetByID(ctx, w.b.ID(), w.inB.ID())
		readDur = time.Since(began)
	}()
	close(start)
	wg.Wait()

	failed := 0
	for _, e := range errs {
		if e != nil {
			failed++
			t.Logf("guard run failed: %v", e)
		}
	}
	if failed != 0 {
		t.Fatalf("%d of %d concurrent guard runs failed on a pool of %d: the guard must never wait for a second connection", failed, runs, poolSize)
	}
	if readErr != nil || readDur > deadline/2 {
		t.Fatalf("an unrelated read answered %v after %s while the guards ran", readErr, readDur)
	}
	for _, p := range people {
		if got, err := w.persons.GetByID(t.Context(), p.ID()); err != nil || !got.IsGloballySuspended() {
			t.Fatalf("person %s: err=%v, want suspended", p.ID(), err)
		}
	}
}
