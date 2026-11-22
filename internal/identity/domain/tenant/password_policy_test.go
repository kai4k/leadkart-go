package tenant_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestPasswordPolicy_Check(t *testing.T) {
	t.Parallel()
	strict := tenant.DefaultPasswordPolicy()
	lenient, err := tenant.NewPasswordPolicy(8, false, false, false, false, 5, 15)
	if err != nil {
		t.Fatal(err)
	}
	longPolicy, err := tenant.NewPasswordPolicy(128, false, false, false, false, 5, 15)
	if err != nil {
		t.Fatal(err)
	}
	allFour := []string{tenant.PasswordRuleMinLength, tenant.PasswordRuleUppercase, tenant.PasswordRuleDigit, tenant.PasswordRuleSymbol}
	cases := []struct {
		name   string
		policy tenant.PasswordPolicy
		pw     string
		rules  []string
	}{
		{"strict ok", strict, "Correct-Horse-9!", nil},
		{"strict short+classes", strict, "abc", allFour},
		{"spaces are not symbols", strict, "Correct Horse Battery 9", []string{tenant.PasswordRuleSymbol}},
		{"missing lowercase is reported", strict, "CORRECT-HORSE-9!", []string{tenant.PasswordRuleLowercase}},
		{"unicode symbol counts", strict, "Correct€Horse9Battery", nil},
		{"lenient length only", lenient, "abcdefgh", nil},
		{"lenient still has a floor", lenient, "abcdefg", []string{tenant.PasswordRuleMinLength}},
		{"zero policy = default", tenant.PasswordPolicy{}, "abc", allFour},
		{"length is counted in runes, not bytes", lenient, "ééééééé", []string{tenant.PasswordRuleMinLength}},
		{"eight runes of two bytes are eight", lenient, "éééééééé", nil},
		{"the longest accepted password", longPolicy, strings.Repeat("a", tenant.MaxPasswordBytes), nil},
		{"one byte over the bound", longPolicy, strings.Repeat("a", tenant.MaxPasswordBytes+1), []string{tenant.PasswordRuleMaxLength}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.policy.Check(tc.pw)
			var v *tenant.PasswordPolicyViolation
			if tc.rules == nil {
				if err != nil {
					t.Fatalf("unexpected %v", err)
				}
				return
			}
			if !errors.As(err, &v) || !errors.Is(err, tenant.ErrPasswordPolicy) || !slices.Equal(v.Rules, tc.rules) {
				t.Fatalf("got %v, want rules %v", err, tc.rules)
			}
		})
	}
}

// The bound is checked before any counting, and the message never carries the
// password.
func TestPasswordPolicy_Check_OversizeAnswersOnlyTheBoundAndNeverEchoes(t *testing.T) {
	t.Parallel()
	pw := strings.Repeat("Ab1!", tenant.MaxPasswordBytes) // far over the bound
	err := tenant.DefaultPasswordPolicy().Check(pw)
	var v *tenant.PasswordPolicyViolation
	if !errors.As(err, &v) || !slices.Equal(v.Rules, []string{tenant.PasswordRuleMaxLength}) {
		t.Fatalf("got %v, want only max_length", err)
	}
	if strings.Contains(err.Error(), "Ab1!") {
		t.Fatalf("the error echoes the password: %q", err.Error())
	}
}

func TestPasswordPolicy_Check_ErrorListsRulesNotThePassword(t *testing.T) {
	t.Parallel()
	err := tenant.DefaultPasswordPolicy().Check("hunter2")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %v must not echo the password", err)
	}
	if !strings.Contains(err.Error(), tenant.PasswordRuleMinLength) {
		t.Fatalf("error %q should list the failed rules", err.Error())
	}
}

// Owner ruling 2026-11-21: the default is 8 characters with all four classes, equal to
// the validated floor. A tenant with no stored policy (the zero policy) is checked as
// the default, so it picks the new default up; a stored 12 stays 12.
func TestDefaultPasswordPolicy_IsEightCharactersWithFourClasses(t *testing.T) {
	t.Parallel()
	d := tenant.DefaultPasswordPolicy()
	if d.MinLength() != 8 {
		t.Fatalf("default MinLength = %d, want 8", d.MinLength())
	}
	if _, err := tenant.NewPasswordPolicy(8, true, true, true, true, 5, 15); err != nil {
		t.Fatalf("the default's length is not the validated floor: %v", err)
	}
	if _, err := tenant.NewPasswordPolicy(7, true, true, true, true, 5, 15); err == nil {
		t.Fatal("a stored policy below 8 must stay refused")
	}
	if !d.RequireUppercase() || !d.RequireLowercase() || !d.RequireDigit() || !d.RequireSymbol() {
		t.Fatalf("the four classes stay required: %+v", d)
	}

	twelve, err := tenant.NewPasswordPolicy(12, true, true, true, true, 5, 15)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		policy tenant.PasswordPolicy
		pw     string
		rules  []string
	}{
		{"default accepts 8 with four classes", d, "Ab1!cdef", nil},
		{"default refuses 7 with four classes", d, "Ab1!cde", []string{tenant.PasswordRuleMinLength}},
		{"a tenant with no stored policy accepts 8", tenant.PasswordPolicy{}, "Ab1!cdef", nil},
		{"a tenant with no stored policy refuses 7", tenant.PasswordPolicy{}, "Ab1!cde", []string{tenant.PasswordRuleMinLength}},
		{"a stored 12 stays 12 (8 refused)", twelve, "Ab1!cdef", []string{tenant.PasswordRuleMinLength}},
		{"a stored 12 accepts 12", twelve, "Ab1!cdefghij", nil},
		{"8 without the classes is still refused", d, "abcdefgh", []string{tenant.PasswordRuleUppercase, tenant.PasswordRuleDigit, tenant.PasswordRuleSymbol}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.policy.Check(tc.pw)
			var v *tenant.PasswordPolicyViolation
			if tc.rules == nil {
				if err != nil {
					t.Fatalf("unexpected %v", err)
				}
				return
			}
			if !errors.As(err, &v) || !slices.Equal(v.Rules, tc.rules) {
				t.Fatalf("got %v, want rules %v", err, tc.rules)
			}
		})
	}
}

// A tenant with no stored policy HAS the default policy: the one accessor every
// read goes through, so no reader can emit the zero value.
func TestSettings_EffectivePasswordPolicy_ZeroIsTheDefaultAndAStoredPolicyIsKept(t *testing.T) {
	t.Parallel()
	if got, want := (tenant.Settings{}).EffectivePasswordPolicy(), tenant.DefaultPasswordPolicy(); got != want {
		t.Fatalf("zero settings effective policy = %+v, want the default %+v", got, want)
	}
	stored, err := tenant.NewPasswordPolicy(12, true, false, true, false, 7, 30)
	if err != nil {
		t.Fatalf("NewPasswordPolicy: %v", err)
	}
	if got := tenant.NewSettings(stored).EffectivePasswordPolicy(); got != stored {
		t.Fatalf("stored policy effective = %+v, want %+v", got, stored)
	}
	// The raw accessor stays raw: persistence must not write the default back.
	if !(tenant.Settings{}).PasswordPolicy().IsZero() {
		t.Fatal("PasswordPolicy() must stay the stored value so a save does not freeze the default")
	}
}
