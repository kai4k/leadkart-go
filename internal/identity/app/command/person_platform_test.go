package command_test

import (
	"time"

	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)


func TestGlobalSuspendPerson_Succeeds(t *testing.T) {
	t.Parallel()
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}
	h := command.NewGlobalSuspendPersonHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.GlobalSuspendPersonCommand{
		PersonID: repo.person.ID(),
		Reason:   "compliance: cross-tenant abuse 2026-05-07",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !repo.person.IsGloballySuspended() {
		t.Error("expected Person globally suspended")
	}
}

func TestGlobalSuspendPerson_RequiresReason(t *testing.T) {
	t.Parallel()
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}
	h := command.NewGlobalSuspendPersonHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.GlobalSuspendPersonCommand{
		PersonID: repo.person.ID(),
	})
	if !errors.Is(err, person.ErrInvalid) {
		t.Fatalf("err = %v, want wraps person.ErrInvalid", err)
	}
}

func TestLiftPersonGlobalSuspension_RoundTrip(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	if err := p.GloballySuspend("temp-ban", testNow); err != nil {
		t.Fatalf("setup GloballySuspend: %v", err)
	}
	p.PullEvents()
	repo := &fakePersonRepo{person: p}

	h := command.NewLiftPersonGlobalSuspensionHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.LiftPersonGlobalSuspensionCommand{
		PersonID: p.ID(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if p.IsGloballySuspended() {
		t.Error("expected suspension lifted")
	}
}

func TestAnonymisePerson_Succeeds(t *testing.T) {
	t.Parallel()
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}
	h := command.NewAnonymisePersonHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.AnonymisePersonCommand{
		PersonID: repo.person.ID(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !repo.person.IsAnonymised() {
		t.Error("expected Person anonymised")
	}
}

func TestUpdatePersonProfile_Succeeds(t *testing.T) {
	t.Parallel()
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}
	h := command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.UpdatePersonProfileCommand{
		PersonID:  repo.person.ID(),
		FirstName: "Renamed",
		LastName:  "Surname",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if repo.person.FirstName() != "Renamed" {
		t.Errorf("FirstName = %q, want Renamed", repo.person.FirstName())
	}
	if repo.person.LastName() != "Surname" {
		t.Errorf("LastName = %q, want Surname", repo.person.LastName())
	}
}

func TestPersonPlatformHandlers_NotFound(t *testing.T) {
	t.Parallel()
	repo := &fakePersonRepo{} // no Person seeded
	bad := person.ID("99999999-9999-9999-9999-999999999999")
	cases := []struct {
		name string
		fn   func() error
	}{
		{"GlobalSuspend", func() error {
			return command.NewGlobalSuspendPersonHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.GlobalSuspendPersonCommand{PersonID: bad, Reason: "x"})
		}},
		{"LiftSuspension", func() error {
			return command.NewLiftPersonGlobalSuspensionHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.LiftPersonGlobalSuspensionCommand{PersonID: bad})
		}},
		{"Anonymise", func() error {
			return command.NewAnonymisePersonHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.AnonymisePersonCommand{PersonID: bad})
		}},
		{"UpdateProfile", func() error {
			return command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.UpdatePersonProfileCommand{PersonID: bad, FirstName: "x", LastName: "y"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.fn()
			if !errors.Is(err, command.ErrPersonNotFound) {
				t.Errorf("err = %v, want ErrPersonNotFound", err)
			}
		})
	}
}
