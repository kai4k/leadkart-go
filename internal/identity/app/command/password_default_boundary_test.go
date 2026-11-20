package command_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Owner ruling 2026-11-21: the default password policy is 8 characters minimum with an
// upper-case letter, a lower-case letter, a digit and a symbol (it was 12 and the same
// four classes). The default now equals the policy's validated floor (8). The boundary
// is pinned for every command that sets a password, against a tenant with NO stored
// policy (the default applies).
const (
	passwordEightFourClasses = "Ab1!cdef" // 8 characters: upper, lower, digit, symbol
	passwordSevenFourClasses = "Ab1!cde"  // 7 characters, the same classes
)

func TestDefaultPassword_RegisterTenant_EightIsAcceptedSevenIsRefused(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	h := b2NewRegisterHandler(deps)

	short := b2RegisterCmd(t)
	short.AdminPassword = passwordSevenFourClasses
	_, err := h.Handle(t.Context(), short)
	requireViolation(t, err, tenant.PasswordRuleMinLength)

	ok := b2RegisterCmd(t)
	ok.AdminPassword = passwordEightFourClasses
	if _, err := h.Handle(t.Context(), ok); err != nil {
		t.Fatalf("8 characters with four classes = %v, want accepted", err)
	}
}

func TestDefaultPassword_CreateUser_EightIsAcceptedSevenIsRefused(t *testing.T) {
	t.Parallel()
	strictID := tenant.ID("22222222-2222-2222-2222-222222222222") // no stored policy: the default applies
	persons := seedPersonRepo(t, nil)
	h := newCreateUserHandlerT(fakeUoW{}, persons, newFakeMembershipRepo(), defaultTenantRepo(t), policyNow, testNewPersonID, testNewMembershipID)
	addr, _ := email.New("eight@example.test")

	_, err := h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t), TenantID: strictID, Email: addr, Password: passwordSevenFourClasses, FirstName: "E", LastName: "T"})
	requireViolation(t, err, tenant.PasswordRuleMinLength)
	if _, err := h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t), TenantID: strictID, Email: addr, Password: passwordEightFourClasses, FirstName: "E", LastName: "T"}); err != nil {
		t.Fatalf("8 characters with four classes = %v, want accepted", err)
	}
}

func TestDefaultPassword_ChangePassword_EightIsAcceptedSevenIsRefused(t *testing.T) {
	t.Parallel()
	const current = "Real-Current-Pw-9!"
	strictID := tenant.ID("22222222-2222-2222-2222-222222222222")
	for _, tc := range []struct {
		name  string
		newPw string
		ok    bool
	}{
		{"seven refused", passwordSevenFourClasses, false},
		{"eight accepted", passwordEightFourClasses, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPersonWithPassword(t, current)
			h := command.NewChangePasswordHandler(seedPersonRepo(t, p), refreshtokentest.NewFakeRepository(), defaultTenantRepo(t), fakeUoW{}, passwordpolicy.Noop{}, policyNow)
			err := h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), TenantID: strictID, CurrentPassword: current, NewPassword: tc.newPw})
			if tc.ok {
				if err != nil {
					t.Fatalf("8 characters with four classes = %v, want accepted", err)
				}
				return
			}
			requireViolation(t, err, tenant.PasswordRuleMinLength)
		})
	}
}

func TestDefaultPassword_ConfirmPasswordReset_EightIsAcceptedSevenIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		newPw string
		ok    bool
	}{
		{"seven refused", passwordSevenFourClasses, false},
		{"eight accepted", passwordEightFourClasses, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, _ := email.New("alice@example.test")
			p := newPersonWithPassword(t, "current-pw")
			repo := newResettableRepo(t, p)
			if err := command.NewRequestPasswordResetHandler(repo, policyNow).Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
				t.Fatal(err)
			}
			raw := repo.emailRequestedToken(t)
			h := command.NewConfirmPasswordResetHandler(repo, refreshtokentest.NewFakeRepository(), membershiptest.NewFakeRepository(), defaultTenantRepo(t), fakeUoW{}, passwordpolicy.Noop{}, policyNow)
			err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: raw, NewPassword: tc.newPw})
			if tc.ok {
				if err != nil {
					t.Fatalf("8 characters with four classes = %v, want accepted", err)
				}
				return
			}
			requireViolation(t, err, tenant.PasswordRuleMinLength)
		})
	}
}
