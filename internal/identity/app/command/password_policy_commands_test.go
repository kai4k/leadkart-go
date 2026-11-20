package command_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// Task 12 (ADR 0068 D13): the tenant password policy is enforced by every
// command that sets a password, BEFORE anything is hashed.

var policyNow = func() time.Time { return testNow }

func requireViolation(t *testing.T, err error, rules ...string) {
	t.Helper()
	var v *tenant.PasswordPolicyViolation
	if !errors.As(err, &v) || !errors.Is(err, tenant.ErrPasswordPolicy) {
		t.Fatalf("err = %v, want a password policy violation", err)
	}
	if strings.Join(v.Rules, ",") != strings.Join(rules, ",") {
		t.Fatalf("rules = %v, want %v", v.Rules, rules)
	}
}

func TestRegisterTenant_RefusesAPasswordThatFailsTheDefaultPolicy(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	h := b2NewRegisterHandler(deps).WithArgonGate(saturatedGate(t))
	cmd := b2RegisterCmd(t)
	cmd.AdminPassword = "weak"

	_, err := h.Handle(t.Context(), cmd)
	// The saturated gate proves the policy is checked before any hashing: the
	// answer is the violation, not ErrBusy.
	requireViolation(t, err, tenant.PasswordRuleMinLength, tenant.PasswordRuleUppercase, tenant.PasswordRuleDigit, tenant.PasswordRuleSymbol)
	all, lerr := deps.tenants.ListAll(t.Context())
	if lerr != nil || len(all) != 0 {
		t.Fatalf("a refused registration adds no tenant: %d %v", len(all), lerr)
	}
}

func TestCreateUser_EnforcesTheTargetTenantsPolicy(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("recruit@example.test")
	lenient, lenientID := lenientTenantRepo(t)
	strictID := tenant.ID("22222222-2222-2222-2222-222222222222") // default policy
	for _, tn := range []tenant.ID{lenientID, strictID} {
		if _, err := lenient.GetByID(t.Context(), tn); err != nil {
			t.Fatalf("seed %s: %v", tn, err)
		}
	}
	persons := seedPersonRepo(t, nil)
	h := newCreateUserHandlerT(fakeUoW{}, persons, newFakeMembershipRepo(), lenient, policyNow, testNewPersonID, testNewMembershipID)

	// Length-only tenant: "short" (5) fails, "longenough" (10) passes.
	_, err := h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t), TenantID: lenientID, Email: addr, Password: "short", FirstName: "R", LastName: "T"})
	requireViolation(t, err, tenant.PasswordRuleMinLength)
	if _, err := persons.GetByEmail(t.Context(), addr); !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("a refused create must not add the person: %v", err)
	}
	if _, err := h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t), TenantID: lenientID, Email: addr, Password: "longenough", FirstName: "R", LastName: "T"}); err != nil {
		t.Fatalf("lenient tenant, compliant password: %v", err)
	}

	// The default-policy tenant refuses the same password (owner ruling 2026-11-21: the
	// default length is now 8, so the ten characters of "longenough" no longer fail
	// min_length; the missing classes still refuse it).
	other, _ := email.New("other@example.test")
	_, err = h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t), TenantID: strictID, Email: other, Password: "longenough", FirstName: "O", LastName: "T"})
	requireViolation(t, err, tenant.PasswordRuleUppercase, tenant.PasswordRuleDigit, tenant.PasswordRuleSymbol)
}

func TestCreateUser_PolicyIsCheckedBeforeHashing(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("recruit@example.test")
	h := newCreateUserHandlerT(fakeUoW{}, seedPersonRepo(t, nil), newFakeMembershipRepo(), defaultTenantRepo(t), policyNow, testNewPersonID, testNewMembershipID).
		WithArgonGate(saturatedGate(t))
	_, err := h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t),
		TenantID: tenant.ID("11111111-1111-1111-1111-111111111111"), Email: addr,
		Password: strings.Repeat("Ab1!", 600), FirstName: "R", LastName: "T",
	})
	requireViolation(t, err, tenant.PasswordRuleMaxLength)
}

func TestCreateUser_UnknownTenantIsAnErrorNotASilentDefault(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("recruit@example.test")
	h := newCreateUserHandlerT(fakeUoW{}, seedPersonRepo(t, nil), newFakeMembershipRepo(), tenanttest.NewFakeRepository(), policyNow, testNewPersonID, testNewMembershipID)
	_, err := h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t),
		TenantID: tenant.ID("33333333-3333-3333-3333-333333333333"), Email: addr, Password: "Tr0ub4dor&3-fresh-passphrase",
	})
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("err = %v, want tenant.ErrNotFound", err)
	}
}

func TestChangePassword_EnforcesTheCallersTenantPolicy(t *testing.T) {
	t.Parallel()
	const current = "Real-Current-Pw-9!"
	lenient, lenientID := lenientTenantRepo(t)
	strictID := tenant.ID("22222222-2222-2222-2222-222222222222")

	cases := []struct {
		name     string
		tenantID tenant.ID
		newPw    string
		rules    []string // nil = accepted
	}{
		{"strict tenant refuses a password without the classes (10 characters meet the 8-character default)", strictID, "abcdefgh12", []string{tenant.PasswordRuleUppercase, tenant.PasswordRuleSymbol}},
		{"lenient tenant accepts it", lenientID, "abcdefgh12", nil},
		{"no tenant context applies the default", "", "abcdefgh12", []string{tenant.PasswordRuleUppercase, tenant.PasswordRuleSymbol}},
		{"a tenant that is gone applies the default", tenant.ID("33333333-3333-3333-3333-333333333333"), "abcdefgh12", []string{tenant.PasswordRuleUppercase, tenant.PasswordRuleSymbol}},
		{"a compliant password passes the default", strictID, "Brand-New-Passphrase-9!", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPersonWithPassword(t, current)
			stamp := p.SecurityStamp().String()
			repo := seedPersonRepo(t, p)
			h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), lenient, fakeUoW{}, passwordpolicy.Noop{}, policyNow)
			err := h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), TenantID: tc.tenantID, CurrentPassword: current, NewPassword: tc.newPw})
			if tc.rules == nil {
				if err != nil {
					t.Fatalf("unexpected %v", err)
				}
				return
			}
			requireViolation(t, err, tc.rules...)
			if p.SecurityStamp().String() != stamp {
				t.Fatal("a refused password must not rotate the stamp")
			}
		})
	}
}

// The policy is checked before the current password is verified and before any
// hashing: with every Argon2 slot taken, a violating new password and an oversize
// current password are answered without waiting for one.
func TestChangePassword_PolicyAndSizeBoundComeBeforeAnyHashing(t *testing.T) {
	t.Parallel()
	const current = "Real-Current-Pw-9!"
	p := newPersonWithPassword(t, current)
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, policyNow).
		WithArgonGate(saturatedGate(t))

	err := h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), CurrentPassword: current, NewPassword: "weak"})
	requireViolation(t, err, tenant.PasswordRuleMinLength, tenant.PasswordRuleUppercase, tenant.PasswordRuleDigit, tenant.PasswordRuleSymbol)

	err = h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), CurrentPassword: current, NewPassword: strings.Repeat("Ab1!", 600)})
	requireViolation(t, err, tenant.PasswordRuleMaxLength)

	err = h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), CurrentPassword: strings.Repeat("x", tenant.MaxPasswordBytes+1), NewPassword: "Brand-New-Passphrase-9!"})
	if !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("an oversize current password: %v, want ErrIncorrectCurrentPassword without hashing", err)
	}

	// A compliant request still meets the saturated gate: the checks above are not
	// short-circuiting everything.
	err = h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), CurrentPassword: current, NewPassword: "Brand-New-Passphrase-9!"})
	if !errors.Is(err, argon2.ErrBusy) {
		t.Fatalf("compliant request: %v, want ErrBusy", err)
	}
}

// A person holds at most one active membership (409 person_active_elsewhere), so
// there is nothing to choose between: the reset reads that membership's tenant.
func TestConfirmPasswordReset_AppliesThePolicyOfTheActiveMembershipsTenant(t *testing.T) {
	t.Parallel()
	lenient, lenientID := lenientTenantRepo(t)
	strictID := tenant.ID("22222222-2222-2222-2222-222222222222")

	cases := []struct {
		name     string
		tenantID tenant.ID // active membership's tenant; "" = no membership
		newPw    string
		rules    []string
	}{
		{"strict tenant", strictID, "abcdefgh12", []string{tenant.PasswordRuleUppercase, tenant.PasswordRuleSymbol}},
		{"lenient tenant", lenientID, "abcdefgh12", nil},
		{"no active membership applies the default", "", "abcdefgh12", []string{tenant.PasswordRuleUppercase, tenant.PasswordRuleSymbol}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, _ := email.New("alice@example.test")
			p := newPersonWithPassword(t, "current-pw")
			repo := newResettableRepo(t, p)
			members := membershiptest.NewFakeRepository()
			if tc.tenantID != "" {
				tn, err := lenient.GetByID(t.Context(), tc.tenantID)
				if err != nil {
					t.Fatalf("tenant: %v", err)
				}
				if err := members.Add(t.Context(), activeMembership(t, p, tn)); err != nil {
					t.Fatalf("membership: %v", err)
				}
			}
			if err := command.NewRequestPasswordResetHandler(repo, policyNow).Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
				t.Fatal(err)
			}
			raw := repo.emailRequestedToken(t)
			stamp := p.SecurityStamp().String()

			h := command.NewConfirmPasswordResetHandler(repo, refreshtokentest.NewFakeRepository(), members, lenient, fakeUoW{}, passwordpolicy.Noop{}, policyNow)
			err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: raw, NewPassword: tc.newPw})
			if tc.rules == nil {
				if err != nil {
					t.Fatalf("unexpected %v", err)
				}
				return
			}
			requireViolation(t, err, tc.rules...)
			// A refused password spends nothing: the stamp is unchanged and the
			// token still works for a compliant one.
			if p.SecurityStamp().String() != stamp || p.PendingPasswordReset().IsZero() {
				t.Fatal("a refused password must leave the pending reset intact")
			}
			if err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: raw, NewPassword: "Brand-New-Passphrase-9!"}); err != nil {
				t.Fatalf("retry with a compliant password: %v", err)
			}
		})
	}
}

// The policy answer is reachable only with a valid token: with a bad token the
// answer is the same ErrResetTokenInvalid whether the password is weak or not.
func TestConfirmPasswordReset_PolicyAnswerNeedsAValidToken(t *testing.T) {
	t.Parallel()
	repo := newResettableRepo(t, newPersonWithPassword(t, "current-pw"))
	h := command.NewConfirmPasswordResetHandler(repo, refreshtokentest.NewFakeRepository(), membershiptest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, policyNow).
		WithArgonGate(saturatedGate(t))
	for _, pw := range []string{"weak", "Brand-New-Passphrase-9!", strings.Repeat("Ab1!", 600)} {
		err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: "no-such-token", NewPassword: pw})
		if !errors.Is(err, command.ErrResetTokenInvalid) {
			t.Fatalf("password %.8q with a bad token: %v, want ErrResetTokenInvalid", pw, err)
		}
	}
}

func TestConfirmPasswordReset_PolicyIsCheckedBeforeHashing(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(repo, policyNow).Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatal(err)
	}
	h := command.NewConfirmPasswordResetHandler(repo, refreshtokentest.NewFakeRepository(), membershiptest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, policyNow).
		WithArgonGate(saturatedGate(t))
	err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: repo.emailRequestedToken(t), NewPassword: strings.Repeat("Ab1!", 600)})
	requireViolation(t, err, tenant.PasswordRuleMaxLength)
}
