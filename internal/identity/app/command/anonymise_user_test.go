package command_test

import (
	"time"

	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func TestAnonymiseUser_Cascades(t *testing.T) {
	t.Parallel()
	mRepo := newFakeMembershipRepo()
	pRepo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}

	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		pRepo.person.ID(),
		tenant.ID("33333333-3333-3333-3333-333333333333"),
		membership.ID(""),
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	m.PullEvents()
	_ = mRepo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup

	h := command.NewAnonymiseUserHandler(mRepo, pRepo, func() time.Time { return testNow })
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
	h := command.NewAnonymiseUserHandler(mRepo, pRepo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.AnonymiseUserCommand{
		MembershipID: membership.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}
