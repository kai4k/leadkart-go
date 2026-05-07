package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
)

func TestAnonymiseUser_Cascades(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	mRepo := newFakeMembershipRepo()
	pRepo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}

	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		pRepo.person.ID(),
		"33333333-3333-3333-3333-333333333333",
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	m.PullEvents()
	_ = mRepo.Add(t.Context(), m)

	h := command.NewAnonymiseUserHandler(mRepo, pRepo)
	if err := h.Handle(t.Context(), command.AnonymiseUserCommand{MembershipID: m.ID()}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !pRepo.person.IsAnonymised() {
		t.Error("expected Person anonymised")
	}
}

func TestAnonymiseUser_NotFound(t *testing.T) {
	t.Parallel()
	mRepo := newFakeMembershipRepo()
	pRepo := &fakePersonRepo{}
	h := command.NewAnonymiseUserHandler(mRepo, pRepo)
	err := h.Handle(t.Context(), command.AnonymiseUserCommand{
		MembershipID: membership.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}
