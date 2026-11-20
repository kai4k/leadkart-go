//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context(); no goroutines.
//
// arch-test:no-synctest — HTTP round trips and Postgres; no time-dependent logic
// beyond the fixed injected clock.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/obs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

const (
	strongPw     = "Correct-Horse-Battery-9!"
	weakPw       = "weak"
	lenientPw    = "abcdefgh"
	policyDetail = "password does not satisfy the tenant password policy"
)

var weakRules = []string{"min_length", "requires_uppercase", "requires_digit", "requires_symbol"}

func (f e2eFixture) requirePolicyViolation(t *testing.T, r httpResp, field string, want []string) {
	t.Helper()
	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d %s", r.status, r.body)
	}
	er := decodeError(t, r.body)
	if er.Error != "password_policy_violation" || !slices.Equal(er.Errors[field], want) || len(er.Errors) != 1 {
		t.Fatalf("body: %s (want %s: %v)", r.body, field, want)
	}
	if strings.Contains(string(r.body), weakPw+"\"") || strings.Contains(string(r.body), strongPw) || er.Detail != policyDetail {
		t.Fatalf("the answer must carry rule codes and a fixed detail, never the password: %s", r.body)
	}
}

func (f e2eFixture) setPolicy(t *testing.T, tn registeredTenant, minLen int, classes bool) {
	t.Helper()
	r := f.authedJSON(t, http.MethodPatch, "/api/v1/tenants/"+tn.TenantID+"/settings", tn.AccessToken,
		ports.UpdateTenantSettingsRequest{PasswordPolicy: ports.PasswordPolicyDto{
			MinLength: minLen, RequireUppercase: classes, RequireLowercase: classes, RequireDigit: classes, RequireSymbol: classes,
			MaxFailedAttempts: 5, LockoutMinutes: 15,
		}})
	if r.status != http.StatusNoContent {
		t.Fatalf("set policy: %d %s", r.status, r.body)
	}
}

func TestE2E_PasswordPolicy_EnforcedOnCreateUser(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pwpol")
	req := ports.CreateUserRequest{Email: "weak@e2e.test", Password: weakPw, FirstName: "W", LastName: "K"}
	f.requirePolicyViolation(t, f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken, req), "password", weakRules)

	// Nothing was created: the same address is free for a compliant temporary password.
	req.Password = strongPw
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken, req); r.status != http.StatusCreated {
		t.Fatalf("compliant create: %d %s", r.status, r.body)
	}
}

func TestE2E_PasswordPolicy_CreateUserUsesTheTargetTenantsPolicy(t *testing.T) {
	f := newE2EFixture(t)
	lenient := f.registerAndLogin(t, "pwlen")
	strict := f.registerAndLogin(t, "pwstr")
	f.setPolicy(t, lenient, 8, false)

	// The same password: accepted where the policy is length-only, refused where it is the
	// default (owner ruling 2026-11-21: 8 characters are long enough for the default now,
	// so only the missing classes refuse it).
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", lenient.AccessToken,
		ports.CreateUserRequest{Email: "len@e2e.test", Password: lenientPw, FirstName: "L", LastName: "N"}); r.status != http.StatusCreated {
		t.Fatalf("lenient tenant: %d %s", r.status, r.body)
	}
	r := f.authedJSON(t, http.MethodPost, "/api/v1/users", strict.AccessToken,
		ports.CreateUserRequest{Email: "str@e2e.test", Password: lenientPw, FirstName: "S", LastName: "T"})
	f.requirePolicyViolation(t, r, "password", []string{"requires_uppercase", "requires_digit", "requires_symbol"})
}

func TestE2E_PasswordPolicy_ChangePassword(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pwchg")

	r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", owner.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: owner.Password, NewPassword: weakPw})
	f.requirePolicyViolation(t, r, "new_password", weakRules)
	// Nothing changed: the session and the old password both still work.
	if s := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", owner.AccessToken, nil); s.status != http.StatusOK {
		t.Fatalf("a refused change must leave the session alone: %d %s", s.status, s.body)
	}
	f.loginPair(t, owner.Email, owner.Password)

	// A password far over the size bound is refused the same way, by the bound alone.
	r = f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", owner.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: owner.Password, NewPassword: strings.Repeat("Ab1!", 600)})
	f.requirePolicyViolation(t, r, "new_password", []string{"max_length"})

	if r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", owner.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: owner.Password, NewPassword: strongPw + "x"}); r.status != http.StatusNoContent {
		t.Fatalf("compliant change: %d %s", r.status, r.body)
	}
}

// A weakened policy does not lock anyone out; a strengthened one applies at the
// next password set, not at login.
func TestE2E_PasswordPolicy_ChangesApplyAtTheNextPasswordSet(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pwdrift")
	f.setPolicy(t, owner, 8, false)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken,
		ports.CreateUserRequest{Email: "drift@e2e.test", Password: lenientPw, FirstName: "D", LastName: "R"}); r.status != http.StatusCreated {
		t.Fatalf("create under the lenient policy: %d %s", r.status, r.body)
	}

	// Strengthen to a stored 12 (a tenant that explicitly carries 12 stays at 12 although the
	// default is 8 now): the member's existing weak password still signs in.
	f.setPolicy(t, owner, 12, true)
	pair := f.loginPair(t, "drift@e2e.test", lenientPw)

	// The next password they set is held to the stronger policy.
	r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", pair.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: lenientPw, NewPassword: "abcdefgh12"})
	f.requirePolicyViolation(t, r, "new_password", []string{"min_length", "requires_uppercase", "requires_symbol"})
}

func (f e2eFixture) seedResetToken(t *testing.T, personID string) string {
	t.Helper()
	raw := "reset-" + ids.NewV7().String()
	sum := sha256.Sum256([]byte(raw))
	hash, err := person.NewPasswordResetTokenHash(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.persons.UpdateByID(t.Context(), person.ID(personID), func(p *person.Person) (bool, error) {
		return true, p.RequestPasswordReset(raw, hash, time.Hour, time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	}); err != nil {
		t.Fatalf("seed reset token: %v", err)
	}
	return raw
}

func TestE2E_PasswordPolicy_ResetPassword(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pwreset")
	token := f.seedResetToken(t, owner.PersonID)

	// A weak password with a VALID token: 422 with the rule codes, and the token is not spent.
	f.requirePolicyViolation(t, f.postJSON(t, "/api/v1/auth/reset-password",
		ports.ResetPasswordRequest{Token: token, NewPassword: weakPw}), "new_password", weakRules)

	// A weak and a strong password with an INVALID token answer alike: the policy
	// answer is reachable only by someone who holds a valid token.
	for _, pw := range []string{weakPw, strongPw} {
		r := f.postJSON(t, "/api/v1/auth/reset-password", ports.ResetPasswordRequest{Token: "not-a-token", NewPassword: pw})
		if r.status != http.StatusBadRequest || decodeError(t, r.body).Error != "reset_token_invalid" {
			t.Fatalf("invalid token with %q: %d %s", pw, r.status, r.body)
		}
	}

	// The same token still works with a compliant password.
	if r := f.postJSON(t, "/api/v1/auth/reset-password",
		ports.ResetPasswordRequest{Token: token, NewPassword: "Reset-Pass-2026!ok"}); r.status != http.StatusNoContent {
		t.Fatalf("compliant reset: %d %s", r.status, r.body)
	}
}

// A reset applies the policy of the tenant of the person's active membership.
func TestE2E_PasswordPolicy_ResetUsesTheActiveMembershipsTenant(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pwresetlen")
	f.setPolicy(t, owner, 8, false)
	token := f.seedResetToken(t, owner.PersonID)
	if r := f.postJSON(t, "/api/v1/auth/reset-password",
		ports.ResetPasswordRequest{Token: token, NewPassword: lenientPw}); r.status != http.StatusNoContent {
		t.Fatalf("length-only policy: %d %s", r.status, r.body)
	}
}

func (f e2eFixture) capabilitiesPolicy(t *testing.T, token string, headers map[string]string) ports.PasswordRulesDto {
	t.Helper()
	r := f.doWithHeaders(t, http.MethodGet, "/api/v1/auth/me/capabilities", token, nil, headers)
	if r.status != http.StatusOK {
		t.Fatalf("capabilities: %d %s", r.status, r.body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["password_policy"]; !ok {
		t.Fatalf("capabilities has no password_policy (it is required): %s", r.body)
	}
	var cd ports.CapabilitiesDto
	if err := json.Unmarshal(r.body, &cd); err != nil {
		t.Fatal(err)
	}
	return cd.PasswordPolicy
}

// A2: GET /auth/me/capabilities carries the policy of the caller's OWN tenant: the
// system default until the tenant sets one, read fresh (a change shows on the next
// call, not after a cache window), per tenant, and never an operator's acted-on
// scope.
func TestE2E_PasswordPolicy_CapabilitiesReportsTheCallersOwnTenantsPolicy(t *testing.T) {
	f := newE2EFixture(t)
	lenient := f.registerAndLogin(t, "pwcl")
	strict := f.registerAndLogin(t, "pwcs")
	def := ports.PasswordRulesDto{MinLength: 8, MaxLength: 1024, RequiresUppercase: true, RequiresLowercase: true, RequiresDigit: true, RequiresSymbol: true}

	if got := f.capabilitiesPolicy(t, lenient.AccessToken, nil); got != def {
		t.Fatalf("default policy = %+v, want %+v", got, def)
	}
	f.setPolicy(t, lenient, 8, false)
	wantLenient := ports.PasswordRulesDto{MinLength: 8, MaxLength: 1024}
	if got := f.capabilitiesPolicy(t, lenient.AccessToken, nil); got != wantLenient {
		t.Fatalf("policy right after the change = %+v, want %+v (read fresh, not from the capabilities cache)", got, wantLenient)
	}
	if got := f.capabilitiesPolicy(t, strict.AccessToken, nil); got != def {
		t.Fatalf("another tenant's policy = %+v, want the default", got)
	}
	// A header naming another tenant changes nothing for a tenant member (and the
	// route ignores it for everyone: self-service identity is never scoped).
	if got := f.capabilitiesPolicy(t, strict.AccessToken, map[string]string{"X-Tenant-Id": lenient.TenantID}); got != def {
		t.Fatalf("with X-Tenant-Id naming another tenant = %+v, want the caller's own (default)", got)
	}
	// The rules are the ones change-password enforces: a password that meets exactly
	// the reported length-only rules is accepted for the lenient tenant.
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", lenient.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: lenient.Password, NewPassword: strings.Repeat("a", wantLenient.MinLength)}); r.status >= 400 {
		t.Fatalf("change-password with a password that meets the reported rules = %d %s", r.status, r.body)
	}
}

// An operator acting in a tenant through X-Tenant-Id still reads the policy of its OWN
// tenant (the token's), not the acted-on tenant's: the rules bind the person who is
// setting a password, which is the operator.
func TestE2E_PasswordPolicy_CapabilitiesIgnoresAnOperatorsActedOnScope(t *testing.T) {
	f := newE2EFixture(t)
	target := f.registerAndLogin(t, "pwop")
	f.setPolicy(t, target, 10, false)
	operator := f.mintPlatformTokenWith(t, nil, true)

	got := f.capabilitiesPolicy(t, operator, map[string]string{"X-Tenant-Id": target.TenantID})
	if got.MinLength == 10 || got.RequiresUppercase == false {
		t.Fatalf("an operator acting in a lenient tenant was shown that tenant's rules: %+v", got)
	}
}

// Item 4: the backlog probes a dead worker shows up in run against the real schema: the
// outbox age is the age of the oldest unacked row (0 when none), the phantom count is
// the table's rows. (The staleness arithmetic is the SQL; the outbox table is never
// seeded from a test, per the outbox doctrine.)
func TestE2E_BacklogProbes_ReadTheRealTables(t *testing.T) {
	f := newE2EFixture(t)
	age, rows, err := obs.ProbeBacklog(t.Context(), f.Pool)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if age < 0 || rows < 0 {
		t.Fatalf("negative figures: %v %d", age, rows)
	}
}

// Owner ruling 2026-11-21: the default policy is 8 characters with four classes. A
// tenant that never set a policy accepts an 8-character four-class password on create
// and change, and refuses 7 with min_length alone.
func TestE2E_PasswordPolicy_DefaultIsEightCharactersWithFourClasses(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pweight")
	const eight, seven = "Ab1!cdef", "Ab1!cde"

	req := ports.CreateUserRequest{Email: "seven@e2e.test", Password: seven, FirstName: "S", LastName: "V"}
	f.requirePolicyViolation(t, f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken, req), "password", []string{"min_length"})
	req.Password = eight
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken, req); r.status != http.StatusCreated {
		t.Fatalf("create with 8 characters and four classes: %d %s", r.status, r.body)
	}

	r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", owner.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: owner.Password, NewPassword: seven})
	f.requirePolicyViolation(t, r, "new_password", []string{"min_length"})
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", owner.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: owner.Password, NewPassword: eight}); r.status != http.StatusNoContent {
		t.Fatalf("change to 8 characters and four classes: %d %s", r.status, r.body)
	}
}
