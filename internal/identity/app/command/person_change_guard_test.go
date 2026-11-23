package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
)

// personFor seeds the person behind a membership of the world.
func personFor(t *testing.T, repo *persontest.FakeRepository, id person.ID) *person.Person {
	t.Helper()
	p, err := anyPersons{}.GetByID(t.Context(), id)
	if err != nil {
		t.Fatalf("person: %v", err)
	}
	if err := repo.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	return p
}

func TestPersonChangeGuard_LastOwnerCannotBeSuspended(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	persons := persontest.NewFakeRepository()
	owner1 := w.member(t, w.owner)
	p1 := personFor(t, persons, owner1.PersonID())
	guard := command.NewPersonChangeGuard(fakeUoW{}, w.lock, w.members, w.roles, persons, awClock)
	suspend := command.NewGlobalSuspendPersonHandler(guard, persons, awClock)

	if err := suspend.Handle(t.Context(), command.GlobalSuspendPersonCommand{PersonID: p1.ID(), Reason: "compliance hold"}); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("suspend the only owner: %v, want ErrLastOwnerProtected", err)
	}
	if p1.IsGloballySuspended() {
		t.Fatal("a refused person-level change was applied")
	}
	if w.lock.calls == 0 {
		t.Fatal("the tenant authority lock was not taken")
	}
}

func TestPersonChangeGuard_SecondOwnerAndNoMembershipsAllowTheChange(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	persons := persontest.NewFakeRepository()
	owner1 := w.member(t, w.owner)
	owner2 := w.member(t, w.owner)
	p1 := personFor(t, persons, owner1.PersonID())
	personFor(t, persons, owner2.PersonID())
	guard := command.NewPersonChangeGuard(fakeUoW{}, w.lock, w.members, w.roles, persons, awClock)
	suspend := command.NewGlobalSuspendPersonHandler(guard, persons, awClock)
	if err := suspend.Handle(t.Context(), command.GlobalSuspendPersonCommand{PersonID: p1.ID(), Reason: "compliance hold"}); err != nil {
		t.Fatalf("suspend with a second active owner: %v", err)
	}
	if !p1.IsGloballySuspended() {
		t.Fatal("change not applied")
	}
	// the other owner is now the last one: its person cannot be suspended too
	p2, _ := persons.GetByID(t.Context(), owner2.PersonID())
	if err := suspend.Handle(t.Context(), command.GlobalSuspendPersonCommand{PersonID: p2.ID(), Reason: "compliance hold"}); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("suspend the remaining owner: %v, want ErrLastOwnerProtected", err)
	}
	// a person with no membership at all is simply changed
	lone := personFor(t, persons, person.ID(awID(7777)))
	if err := suspend.Handle(t.Context(), command.GlobalSuspendPersonCommand{PersonID: lone.ID(), Reason: "compliance hold"}); err != nil {
		t.Fatalf("person without memberships: %v", err)
	}
	// a zero guard must never skip the check
	var zero command.PersonChangeGuard
	if err := zero.Run(t.Context(), lone.ID(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("zero guard ran the change")
	}
}

// scriptedMemberships answers ListAllForPerson from a script (the last entry
// repeats), simulating memberships that change between the guard's reads.
type scriptedMemberships struct {
	membership.Repository
	lists [][]*membership.Membership
	calls int
	// ownConnectionInsideTx counts ListAllForPerson calls made INSIDE the guard's
	// transaction: that read opens a connection of its own, which the guard must
	// never do while it holds the tenant authority lock and the person row.
	ownConnectionInsideTx int
	// onTxReadOutsideTx counts on-transaction reads made with no transaction.
	onTxReadOutsideTx int
}

func (s *scriptedMemberships) ListAllForPerson(ctx context.Context, _ person.ID) ([]*membership.Membership, error) {
	if inGuardTx(ctx) {
		s.ownConnectionInsideTx++
	}
	return s.next(), nil
}

// ListActiveRefsForPersonInTx is the same script read on the caller's
// transaction (the next entry, active memberships only).
func (s *scriptedMemberships) ListActiveRefsForPersonInTx(ctx context.Context, _ person.ID) ([]membership.ActiveRef, error) {
	if !inGuardTx(ctx) {
		s.onTxReadOutsideTx++
	}
	var out []membership.ActiveRef
	for _, m := range s.next() {
		if m.Status() == membership.StatusActive {
			out = append(out, membership.ActiveRef{ID: m.ID(), TenantID: m.TenantID()})
		}
	}
	return out, nil
}

func (s *scriptedMemberships) next() []*membership.Membership {
	i := min(s.calls, len(s.lists)-1)
	s.calls++
	return s.lists[i]
}

// guardTxKey marks a context as being inside the guard's unit of work.
type guardTxKey struct{}

func inGuardTx(ctx context.Context) bool { return ctx.Value(guardTxKey{}) != nil }

// markingUoW runs fn with a context that says "inside a transaction", so a
// double can tell a read made under the guard's locks from one made before them.
type markingUoW struct{}

func (markingUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, guardTxKey{}, true))
}

// The guard takes the person row, re-reads the memberships inside the change
// transaction, and starts over when their active set is not the one it checked.
func TestPersonChangeGuard_RestartsWhenTheActiveSetChangedAndNeverAppliesOnAStaleCheck(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	persons := persontest.NewFakeRepository()
	m := w.member(t, w.exec) // an active, non-owner membership: passes the last-owner check
	p := personFor(t, persons, m.PersonID())
	other := w.member(t, w.exec) // stands for the person's membership somewhere else
	personFor(t, persons, other.PersonID())
	for name, c := range map[string]struct {
		script       [][]*membership.Membership
		wantApplied  int
		wantErr      error
		wantListCall int
	}{
		"stable: checked once, re-read once, applied once": {[][]*membership.Membership{{m}}, 1, nil, 2},
		"one reactivation between the check and the change: the first attempt is dropped": {
			[][]*membership.Membership{{}, {m}, {m}, {m}}, 1, nil, 4},
		"another membership became the active one (same count, different member): the first attempt is dropped": {
			[][]*membership.Membership{{m}, {other}, {other}, {other}}, 1, nil, 4},
		"it never settles: nothing is applied": {
			[][]*membership.Membership{{}, {m}, {}, {m}, {}, {m}}, 0, command.ErrPersonMembershipsChanging, 6},
	} {
		repo := &scriptedMemberships{Repository: w.members, lists: c.script}
		guard := command.NewPersonChangeGuard(markingUoW{}, w.lock, repo, w.roles, persons, awClock)
		applied := 0
		err := guard.Run(t.Context(), p.ID(), func(context.Context) error { applied++; return nil })
		if !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
			t.Errorf("%s: err = %v, want %v", name, err, c.wantErr)
		}
		if applied != c.wantApplied {
			t.Errorf("%s: change applied %d times, want %d", name, applied, c.wantApplied)
		}
		if repo.calls != c.wantListCall {
			t.Errorf("%s: memberships listed %d times, want %d", name, repo.calls, c.wantListCall)
		}
		// Under its locks the guard reads on its own transaction, every time: a
		// read that takes a second connection there stalls the pool (review I2).
		if repo.ownConnectionInsideTx != 0 {
			t.Errorf("%s: %d membership read(s) inside the guard's transaction asked for a connection of their own", name, repo.ownConnectionInsideTx)
		}
		if repo.onTxReadOutsideTx != 0 {
			t.Errorf("%s: %d on-transaction read(s) were made outside a transaction", name, repo.onTxReadOutsideTx)
		}
	}
}
