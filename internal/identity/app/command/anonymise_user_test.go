package command_test

import (
	"context"
	"strings"
	"time"

	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// failingMembershipsForAnonymise overrides GetByID to surface errors.
// AnonymiseUserHandler exercises GetByID first; per-method override
// keeps the scope minimal.
type failingMembershipsForAnonymise struct {
	*membershiptest.FakeRepository
	getByIDErr error
}

func (r *failingMembershipsForAnonymise) GetByID(ctx context.Context, tid tenant.ID, id membership.ID) (*membership.Membership, error) {
	if r.getByIDErr != nil {
		return nil, r.getByIDErr
	}
	return r.FakeRepository.GetByID(ctx, tid, id)
}

// failingPersonsForAnonymise overrides UpdateByID to surface errors.
type failingPersonsForAnonymise struct {
	*persontest.FakeRepository
	updateErr error
}

func (r *failingPersonsForAnonymise) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	return r.FakeRepository.UpdateByID(ctx, id, fn)
}

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func TestAnonymiseUser_Cascades(t *testing.T) {
	t.Parallel()
	mRepo := newFakeMembershipRepo()
	p := newPersonWithPassword(t, "irrelevant")
	pRepo := seedPersonRepo(t, p)

	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		p.ID(),
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
	if err := h.Handle(t.Context(), command.AnonymiseUserCommand{
		TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: m.ID(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !p.IsAnonymised() {
		t.Error("expected Person anonymised")
	}
}

func TestAnonymiseUser_NotFound(t *testing.T) {
	t.Parallel()
	mRepo := newFakeMembershipRepo()
	pRepo := seedPersonRepo(t, nil)
	h := command.NewAnonymiseUserHandler(mRepo, pRepo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.AnonymiseUserCommand{
		TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: membership.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

// TestAnonymiseUser_InputRejections — boundary-input table covers
// the early-return guards before any repo is touched.
func TestAnonymiseUser_InputRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  command.AnonymiseUserCommand
	}{
		{
			name: "zero tenant id",
			cmd: command.AnonymiseUserCommand{
				TenantID:     tenant.ID(""),
				MembershipID: membership.ID("11111111-1111-1111-1111-111111111111"),
			},
		},
		{
			name: "zero membership id",
			cmd: command.AnonymiseUserCommand{
				TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
				MembershipID: membership.ID(""),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewAnonymiseUserHandler(newFakeMembershipRepo(), seedPersonRepo(t, nil), func() time.Time { return testNow })
			err := h.Handle(t.Context(), c.cmd)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestAnonymiseUser_GetByIDError_Wrapped — non-NotFound membership
// lookup failure surfaces as `"anonymise_user: load membership: %w"`.
func TestAnonymiseUser_GetByIDError_Wrapped(t *testing.T) {
	t.Parallel()
	mRepo := &failingMembershipsForAnonymise{
		FakeRepository: membershiptest.NewFakeRepository(),
		getByIDErr:     errBoom,
	}
	pRepo := seedPersonRepo(t, nil)
	h := command.NewAnonymiseUserHandler(mRepo, pRepo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.AnonymiseUserCommand{
		TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: membership.ID("11111111-1111-1111-1111-111111111111"),
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
	if errors.Is(err, command.ErrUserNotFound) {
		t.Fatal("generic load error must NOT collapse to ErrUserNotFound")
	}
	if !strings.Contains(err.Error(), "load membership") {
		t.Errorf("err = %v, want contains 'load membership'", err)
	}
}

// TestAnonymiseUser_PersonNotFound_DataIntegrityErrorWrapped — the
// "Membership points at a missing Person" arm. Surfaces as 500 with
// the specific data-integrity error shape (not silently OK).
func TestAnonymiseUser_PersonNotFound_DataIntegrityErrorWrapped(t *testing.T) {
	t.Parallel()
	mRepo := newFakeMembershipRepo()
	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		person.ID("dangling-person-id"),
		tenant.ID("33333333-3333-3333-3333-333333333333"),
		membership.ID(""),
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	_ = m.PullEvents()
	if addErr := mRepo.Add(t.Context(), m); addErr != nil {
		t.Fatalf("seed: %v", addErr)
	}
	// Persons repo is EMPTY — UpdateByID for dangling-person-id returns ErrNotFound.
	pRepo := persontest.NewFakeRepository()

	h := command.NewAnonymiseUserHandler(mRepo, pRepo, func() time.Time { return testNow })
	err = h.Handle(t.Context(), command.AnonymiseUserCommand{
		TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: m.ID(),
	})
	if err == nil {
		t.Fatal("expected data-integrity error, got nil")
	}
	if !strings.Contains(err.Error(), "person") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, want contains 'person ... missing'", err)
	}
}

// TestAnonymiseUser_PersonUpdateOtherError_Wrapped — non-NotFound
// person-update error surfaces as `"anonymise_user: %w"`.
func TestAnonymiseUser_PersonUpdateOtherError_Wrapped(t *testing.T) {
	t.Parallel()
	mRepo := newFakeMembershipRepo()
	p := newPersonWithPassword(t, "irrelevant")
	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		p.ID(),
		tenant.ID("33333333-3333-3333-3333-333333333333"),
		membership.ID(""),
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	_ = m.PullEvents()
	if addErr := mRepo.Add(t.Context(), m); addErr != nil {
		t.Fatalf("seed Add: %v", addErr)
	}

	inner := persontest.NewFakeRepository()
	if addErr := inner.Add(t.Context(), p); addErr != nil {
		t.Fatalf("seed person: %v", addErr)
	}
	pRepo := &failingPersonsForAnonymise{
		FakeRepository: inner,
		updateErr:      errBoom,
	}

	h := command.NewAnonymiseUserHandler(mRepo, pRepo, func() time.Time { return testNow })
	err = h.Handle(t.Context(), command.AnonymiseUserCommand{
		TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: m.ID(),
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
	if !strings.Contains(err.Error(), "anonymise_user") {
		t.Errorf("err = %v, want contains 'anonymise_user'", err)
	}
}
