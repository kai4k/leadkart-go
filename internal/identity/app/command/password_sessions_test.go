package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// Fix round item 1: a credential replacement ends the person's sessions in the
// same unit of work. (Real-transaction behaviour: password_sessions_integration_test.go.)

func seedActiveFamily(t *testing.T, families *refreshtokentest.FakeRepository, owner person.ID) refreshtoken.FamilyID {
	t.Helper()
	pair, err := refreshmint.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	id := refreshtoken.FamilyID(ids.NewV7().String())
	f, err := refreshtoken.NewFamily(id, owner, tenant.ID(ids.NewV7().String()), "unit", pair.Hash, 14*24*time.Hour, testNow)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	if err := families.Add(t.Context(), f); err != nil {
		t.Fatalf("families.Add: %v", err)
	}
	return id
}

func revokeReasonOf(f *refreshtoken.Family) string {
	if f == nil {
		return ""
	}
	return f.RevokeReason()
}

// After Handle returns, every family of the person is revoked with the
// password_changed reason, the caller's included; other people's are untouched.
func TestChangePassword_RevokesEveryFamilyOfThePerson(t *testing.T) {
	t.Parallel()
	currentPlain := "correct horse battery staple"
	p := newPersonWithPassword(t, currentPlain)
	repo := seedPersonRepo(t, p)
	families := refreshtokentest.NewFakeRepository()
	mine := []refreshtoken.FamilyID{seedActiveFamily(t, families, p.ID()), seedActiveFamily(t, families, p.ID())}
	other := seedActiveFamily(t, families, person.ID("someone-else"))

	h := command.NewChangePasswordHandler(repo, families, tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID: p.ID(), CurrentPassword: currentPlain, NewPassword: "Tr0ub4dor&3-newly-strong-passphrase!",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	for _, id := range mine {
		f, err := families.GetByID(t.Context(), id)
		if err != nil || !f.IsRevoked() || f.RevokeReason() != "password_changed" {
			t.Errorf("family %s: err %v reason %q; want revoked, password_changed", id, err, revokeReasonOf(f))
		}
	}
	if f, err := families.GetByID(t.Context(), other); err != nil || f.IsRevoked() {
		t.Errorf("another person's family must be untouched: err %v", err)
	}
}

// A rejected change (wrong current password) ends nothing.
func TestChangePassword_RejectedChangeRevokesNothing(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "correct horse battery staple")
	repo := seedPersonRepo(t, p)
	families := refreshtokentest.NewFakeRepository()
	id := seedActiveFamily(t, families, p.ID())

	h := command.NewChangePasswordHandler(repo, families, tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID: p.ID(), CurrentPassword: "wrong", NewPassword: "Tr0ub4dor&3-newly-strong-passphrase!",
	}); !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("err = %v, want ErrIncorrectCurrentPassword", err)
	}
	if f, err := families.GetByID(t.Context(), id); err != nil || f.IsRevoked() {
		t.Fatalf("a rejected change must not end the session: err %v", err)
	}
}

// Confirming a reset ends every session of the person.
func TestConfirmPasswordReset_RevokesEveryFamilyOfThePerson(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)
	families := refreshtokentest.NewFakeRepository()
	a := seedActiveFamily(t, families, p.ID())
	b := seedActiveFamily(t, families, p.ID())

	if err := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow }).
		Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	h := command.NewConfirmPasswordResetHandler(repo, families, membershiptest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: repo.emailRequestedToken(t), NewPassword: "Tr0ub4dor&3-newly-strong"}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	for _, id := range []refreshtoken.FamilyID{a, b} {
		if f, err := families.GetByID(t.Context(), id); err != nil || !f.IsRevoked() || f.RevokeReason() != "password_reset" {
			t.Errorf("family %s: err %v reason %q; want revoked, password_reset", id, err, revokeReasonOf(f))
		}
	}
}

// A bad token revokes nothing.
func TestConfirmPasswordReset_BadTokenRevokesNothing(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)
	families := refreshtokentest.NewFakeRepository()
	id := seedActiveFamily(t, families, p.ID())
	h := command.NewConfirmPasswordResetHandler(repo, families, membershiptest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: "not-a-real-token", NewPassword: "Tr0ub4dor&3-newly-strong"}); !errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatalf("err = %v, want ErrResetTokenInvalid", err)
	}
	if f, err := families.GetByID(t.Context(), id); err != nil || f.IsRevoked() {
		t.Fatalf("a bad token must not end sessions: err %v", err)
	}
}

// A password change that lands between the rotation and the person read revokes
// the family in the transaction that rotates the stamp. The refresh re-reads the
// family after the person read and refuses: no token for the new stamp comes out
// of a revoked family.
func TestRefresh_FamilyRevokedAfterThePersonRead_IsRejected(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	rig.persons.afterGetByID = func() {
		fams, err := rig.families.ListActiveForPerson(t.Context(), fx.person.ID())
		if err != nil || len(fams) != 1 {
			t.Errorf("setup: active families = %d (%v)", len(fams), err)
			return
		}
		if err := rig.families.FakeRepository.UpdateByID(t.Context(), fams[0].ID(), func(f *refreshtoken.Family) (bool, error) {
			return true, f.Revoke("password_changed", testNow)
		}); err != nil {
			t.Errorf("setup: revoke: %v", err)
		}
	}

	res, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fx.plaintext})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected", err)
	}
	if res.AccessToken != "" {
		t.Fatal("a token was minted from a family revoked after the person read")
	}
}
