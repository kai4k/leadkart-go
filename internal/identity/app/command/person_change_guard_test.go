package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
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

func TestPersonChangeGuard_LastOwnerCannotBeSuspendedOrAnonymised(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	persons := persontest.NewFakeRepository()
	owner1 := w.member(t, w.owner)
	p1 := personFor(t, persons, owner1.PersonID())
	guard := command.NewPersonChangeGuard(fakeUoW{}, w.lock, w.members, w.roles, persons, awClock)
	suspend := command.NewGlobalSuspendPersonHandler(guard, persons, awClock)
	anon := command.NewAnonymisePersonHandler(guard, persons, awClock)

	if err := suspend.Handle(t.Context(), command.GlobalSuspendPersonCommand{PersonID: p1.ID(), Reason: "compliance hold"}); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("suspend the only owner: %v, want ErrLastOwnerProtected", err)
	}
	if err := anon.Handle(t.Context(), command.AnonymisePersonCommand{PersonID: p1.ID()}); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("anonymise the only owner: %v, want ErrLastOwnerProtected", err)
	}
	if p1.IsGloballySuspended() || p1.IsAnonymised() {
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
