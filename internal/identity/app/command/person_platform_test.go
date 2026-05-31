package command_test

import (
	"context"
	"time"

	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
)

// failingPersonsForPlatform injects errors on UpdateByID, shared across the
// platform person handlers.
type failingPersonsForPlatform struct {
	*persontest.FakeRepository
	updateErr error
}

func (r *failingPersonsForPlatform) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	return r.FakeRepository.UpdateByID(ctx, id, fn)
}

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

// TestPersonPlatformHandlers_EmptyPersonID — every handler rejects zero
// PersonID before any repo call.
func TestPersonPlatformHandlers_EmptyPersonID(t *testing.T) {
	t.Parallel()
	repo := persontest.NewFakeRepository()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"GlobalSuspend", func() error {
			return command.NewGlobalSuspendPersonHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.GlobalSuspendPersonCommand{PersonID: person.ID(""), Reason: "x"})
		}},
		{"LiftSuspension", func() error {
			return command.NewLiftPersonGlobalSuspensionHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.LiftPersonGlobalSuspensionCommand{PersonID: person.ID("")})
		}},
		{"Anonymise", func() error {
			return command.NewAnonymisePersonHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.AnonymisePersonCommand{PersonID: person.ID("")})
		}},
		{"UpdateProfile", func() error {
			return command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.UpdatePersonProfileCommand{PersonID: person.ID(""), FirstName: "x", LastName: "y"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.fn()
			if err == nil {
				t.Fatal("expected error for empty PersonID, got nil")
			}
		})
	}
}

// TestPersonPlatformHandlers_AggregateRejection_Wrapped — aggregate rejections
// propagate wrapping person.ErrInvalid.
func TestPersonPlatformHandlers_AggregateRejection_Wrapped(t *testing.T) {
	t.Parallel()

	t.Run("UpdateProfile empty first name", func(t *testing.T) {
		t.Parallel()
		p := newPersonWithPassword(t, "irrelevant")
		repo := seedPersonRepo(t, p)
		h := command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow })
		err := h.Handle(t.Context(), command.UpdatePersonProfileCommand{
			PersonID:  p.ID(),
			FirstName: "", // aggregate rejects
			LastName:  "x",
		})
		if !errors.Is(err, person.ErrInvalid) {
			t.Fatalf("err = %v, want wraps person.ErrInvalid", err)
		}
	})

	t.Run("UpdateProfile empty last name", func(t *testing.T) {
		t.Parallel()
		p := newPersonWithPassword(t, "irrelevant")
		repo := seedPersonRepo(t, p)
		h := command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow })
		err := h.Handle(t.Context(), command.UpdatePersonProfileCommand{
			PersonID:  p.ID(),
			FirstName: "x",
			LastName:  "", // aggregate rejects
		})
		if !errors.Is(err, person.ErrInvalid) {
			t.Fatalf("err = %v, want wraps person.ErrInvalid", err)
		}
	})

	t.Run("UpdateProfile on anonymised person rejected", func(t *testing.T) {
		t.Parallel()
		p := newPersonWithPassword(t, "irrelevant")
		if err := p.Anonymise(testNow); err != nil {
			t.Fatalf("Anonymise setup: %v", err)
		}
		repo := seedPersonRepo(t, p)
		h := command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow })
		err := h.Handle(t.Context(), command.UpdatePersonProfileCommand{
			PersonID:  p.ID(),
			FirstName: "Renamed",
			LastName:  "Anonymised",
		})
		if !errors.Is(err, person.ErrInvalid) {
			t.Fatalf("err = %v, want wraps person.ErrInvalid (cannot update anonymised)", err)
		}
	})
}

// TestPersonPlatformHandlers_GenericRepoError_Wrapped — a non-NotFound
// UpdateByID failure wraps, not collapses to ErrPersonNotFound.
func TestPersonPlatformHandlers_GenericRepoError_Wrapped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func(repo person.Repository) error
	}{
		{"GlobalSuspend", func(repo person.Repository) error {
			return command.NewGlobalSuspendPersonHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.GlobalSuspendPersonCommand{PersonID: person.ID("some-id"), Reason: "x"})
		}},
		{"LiftSuspension", func(repo person.Repository) error {
			return command.NewLiftPersonGlobalSuspensionHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.LiftPersonGlobalSuspensionCommand{PersonID: person.ID("some-id")})
		}},
		{"Anonymise", func(repo person.Repository) error {
			return command.NewAnonymisePersonHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.AnonymisePersonCommand{PersonID: person.ID("some-id")})
		}},
		{"UpdateProfile", func(repo person.Repository) error {
			return command.NewUpdatePersonProfileHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.UpdatePersonProfileCommand{PersonID: person.ID("some-id"), FirstName: "x", LastName: "y"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := &failingPersonsForPlatform{
				FakeRepository: persontest.NewFakeRepository(),
				updateErr:      errBoom,
			}
			err := c.fn(repo)
			if !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want chain includes errBoom", err)
			}
			if errors.Is(err, command.ErrPersonNotFound) {
				t.Fatal("generic repo error must NOT collapse to ErrPersonNotFound")
			}
		})
	}
}
