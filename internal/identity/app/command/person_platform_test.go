package command_test


import (
	"time"

	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
)


func TestGlobalSuspendPerson_Succeeds(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	repo := seedPersonRepo(t, p)
	h := command.NewGlobalSuspendPersonHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.GlobalSuspendPersonCommand{
		PersonID: p.ID(),
		Reason:   "compliance: cross-tenant abuse 2026-05-07",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !p.IsGloballySuspended() {
		t.Error("expected Person globally suspended")
	}
}

func TestGlobalSuspendPerson_RequiresReason(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	repo := seedPersonRepo(t, p)
	h := command.NewGlobalSuspendPersonHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.GlobalSuspendPersonCommand{
		PersonID: p.ID(),
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
	repo := seedPersonRepo(t, p)

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
	p := newPersonWithPassword(t, "irrelevant")
	repo := seedPersonRepo(t, p)
	h := command.NewAnonymisePersonHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.AnonymisePersonCommand{
		PersonID: p.ID(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !p.IsAnonymised() {
		t.Error("expected Person anonymised")
	}
}

func TestUpdatePersonProfile_Succeeds(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	repo := seedPersonRepo(t, p)
	h := command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.UpdatePersonProfileCommand{
		PersonID:  p.ID(),
		FirstName: "Renamed",
		LastName:  "Surname",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if p.FirstName() != "Renamed" {
		t.Errorf("FirstName = %q, want Renamed", p.FirstName())
	}
	if p.LastName() != "Surname" {
		t.Errorf("LastName = %q, want Surname", p.LastName())
	}
}

func TestPersonPlatformHandlers_NotFound(t *testing.T) {
	t.Parallel()
	repo := persontest.NewFakeRepository() // no Person seeded
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
			t.Parallel()
			err := c.fn()
			if !errors.Is(err, command.ErrPersonNotFound) {
				t.Errorf("err = %v, want ErrPersonNotFound", err)
			}
		})
	}
}
