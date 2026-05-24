package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// TestNewRequestEmailChangeHandler_PanicsOnNilDeps locks the wiring
// contract: the persons repository is required. Per ADR 0057 the
// handler intentionally does NOT take an email gateway — delivery
// is via outbox subscriber, not synchronous call.
func TestNewRequestEmailChangeHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil persons repo")
		}
	}()
	_ = command.NewRequestEmailChangeHandler(nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
}

// TestRequestEmailChange_RejectsZeroPersonID exercises the input-
// shape guard before any repository call. Aligned with the typed-
// sentinel pattern used across the change-password flow.
func TestRequestEmailChange_RejectsZeroPersonID(t *testing.T) {
	t.Parallel()
	addr, err := email.New("new@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	repo := &fakePersonRepo{}
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err = h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: person.ID(""),
		NewEmail: addr,
	})
	if err == nil {
		t.Fatal("expected error for zero person id, got nil")
	}
}

// TestRequestEmailChange_RejectsZeroEmail mirrors the person-id
// guard for the email VO.
func TestRequestEmailChange_RejectsZeroEmail(t *testing.T) {
	t.Parallel()
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: repo.person.ID(),
		NewEmail: email.Address{},
	})
	if err == nil {
		t.Fatal("expected error for zero email, got nil")
	}
}

// TestRequestEmailChange_PersonNotFound_ReturnsErrEmailChangeRejected
// proves the generic-rejection invariant: a missing Person collapses
// to the same error as terminal-state / same-email rejections. Per
// security.md "Email change" + Auth0/Okta canon — enumeration safety.
func TestRequestEmailChange_PersonNotFound_ReturnsErrEmailChangeRejected(t *testing.T) {
	t.Parallel()
	addr, err := email.New("new@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	repo := &fakePersonRepo{} // no Person seeded
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err = h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: person.ID("p-missing-1"),
		NewEmail: addr,
	})
	if !errors.Is(err, command.ErrEmailChangeRejected) {
		t.Fatalf("err = %v, want ErrEmailChangeRejected", err)
	}
}

// TestRequestEmailChange_SameAsCurrent_Rejected proves the no-op
// guard fires before the token mint + UpdateByID call. Email
// canon: requesting a change to the current address is collapsed
// into the generic rejection.
func TestRequestEmailChange_SameAsCurrent_Rejected(t *testing.T) {
	t.Parallel()
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "irrelevant")}
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: repo.person.ID(),
		NewEmail: repo.person.Email(),
	})
	if !errors.Is(err, command.ErrEmailChangeRejected) {
		t.Fatalf("err = %v, want ErrEmailChangeRejected", err)
	}
}
