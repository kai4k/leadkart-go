package tenant

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// PasswordPolicy defines per-tenant password rules (security.md). A tenant admin
// may choose any values inside the validated bounds below, INCLUDING rules weaker
// than [DefaultPasswordPolicy] (8 characters, all four classes): the classes may be
// any subset, none included, and MinLength may be as low as 8 (the NIST SP 800-63B
// floor), which is also the default's length. The bounds are floors and ceilings:
// MinLength 8-128, MaxFailedAttempts 3-50, LockoutMinutes 0-1440. A tenant with no
// stored policy (a zero policy: every tenant that never edited it) is checked as the
// default, so it follows the default whenever the default changes; a tenant whose
// stored policy says 12 stays at 12.
//
// MaxFailedAttempts and LockoutMinutes are stored and returned but NOT ENFORCED:
// login uses the fixed platform thresholds ([person.MaxFailedLogins] failures inside
// [person.LockoutWindow], locked for [person.LockoutDuration]); a per-tenant
// threshold would also break the enumeration parity of the phantom counter, which
// has to use the same fixed numbers for every address (ADR 0068 D12). ADR 0069
// item 54 recommends removing them.
type PasswordPolicy struct {
	minLength         int
	requireUppercase  bool
	requireLowercase  bool
	requireDigit      bool
	requireSymbol     bool
	maxFailedAttempts int
	lockoutMinutes    int
}

// Minimum/maximum bounds for PasswordPolicy fields (security.md floors).
const (
	minPasswordLength    = 8
	minMaxFailedAttempts = 3
	maxPasswordLength    = 128 // sanity cap on minLength choice
	maxMaxFailedAttempts = 50  // sanity cap
	maxLockoutMinutes    = 24 * 60
)

// NewPasswordPolicy validates and returns a PasswordPolicy.
// minLength: 8–128; maxFailedAttempts: 3–50; lockoutMinutes: 0–1440
// (0 = admin-only unlock).
func NewPasswordPolicy(minLength int, requireUppercase, requireLowercase, requireDigit, requireSymbol bool, maxFailedAttempts, lockoutMinutes int) (PasswordPolicy, error) {
	if minLength < minPasswordLength {
		return PasswordPolicy{}, fmt.Errorf("%w: minLength %d < %d (NIST floor)", ErrInvalid, minLength, minPasswordLength)
	}
	if minLength > maxPasswordLength {
		return PasswordPolicy{}, fmt.Errorf("%w: minLength %d > %d (sanity cap)", ErrInvalid, minLength, maxPasswordLength)
	}
	if maxFailedAttempts < minMaxFailedAttempts {
		return PasswordPolicy{}, fmt.Errorf("%w: maxFailedAttempts %d < %d", ErrInvalid, maxFailedAttempts, minMaxFailedAttempts)
	}
	if maxFailedAttempts > maxMaxFailedAttempts {
		return PasswordPolicy{}, fmt.Errorf("%w: maxFailedAttempts %d > %d", ErrInvalid, maxFailedAttempts, maxMaxFailedAttempts)
	}
	if lockoutMinutes < 0 {
		return PasswordPolicy{}, fmt.Errorf("%w: lockoutMinutes negative (%d)", ErrInvalid, lockoutMinutes)
	}
	if lockoutMinutes > maxLockoutMinutes {
		return PasswordPolicy{}, fmt.Errorf("%w: lockoutMinutes %d > %d (24h cap)", ErrInvalid, lockoutMinutes, maxLockoutMinutes)
	}
	return PasswordPolicy{
		minLength:         minLength,
		requireUppercase:  requireUppercase,
		requireLowercase:  requireLowercase,
		requireDigit:      requireDigit,
		requireSymbol:     requireSymbol,
		maxFailedAttempts: maxFailedAttempts,
		lockoutMinutes:    lockoutMinutes,
	}, nil
}

// DefaultPasswordPolicy returns the system default: minLength=8 (owner ruling
// 2026-11-21; it was 12, and 8 is also the validated floor), all four character
// classes required, 5 attempts, 15 min lockout (the last two are stored, not enforced).
func DefaultPasswordPolicy() PasswordPolicy {
	return PasswordPolicy{
		minLength:         8,
		requireUppercase:  true,
		requireLowercase:  true,
		requireDigit:      true,
		requireSymbol:     true,
		maxFailedAttempts: 5,
		lockoutMinutes:    15,
	}
}

// MinLength returns the minimum password length.
func (p PasswordPolicy) MinLength() int { return p.minLength }

// RequireUppercase reports whether at least one uppercase character is required.
func (p PasswordPolicy) RequireUppercase() bool { return p.requireUppercase }

// RequireLowercase reports whether at least one lowercase character is required.
func (p PasswordPolicy) RequireLowercase() bool { return p.requireLowercase }

// RequireDigit reports whether at least one digit is required.
func (p PasswordPolicy) RequireDigit() bool { return p.requireDigit }

// RequireSymbol reports whether at least one symbol is required.
func (p PasswordPolicy) RequireSymbol() bool { return p.requireSymbol }

// MaxFailedAttempts returns the lockout threshold.
func (p PasswordPolicy) MaxFailedAttempts() int { return p.maxFailedAttempts }

// LockoutMinutes returns the auto-unlock window; 0 means admin-only unlock.
func (p PasswordPolicy) LockoutMinutes() int { return p.lockoutMinutes }

// LockoutDuration returns LockoutMinutes as a time.Duration.
func (p PasswordPolicy) LockoutDuration() time.Duration {
	return time.Duration(p.lockoutMinutes) * time.Minute
}

// IsZero reports whether the policy is uninitialised. NewPasswordPolicy rejects
// minLength=0, so zero value always means "not yet configured".
func (p PasswordPolicy) IsZero() bool { return p.minLength == 0 }

// Effective returns the policy that binds: p itself when it was configured, the
// system default when p is the zero policy (a tenant that never saved one). A stored
// policy is always whole: [NewPasswordPolicy] is the only constructor, it validates
// every field, and the repository refuses to load a row whose policy columns are
// neither all zero nor all valid, so there is no partially valid policy to resolve.
// This is the ONE place "zero means default" is decided; enforcement ([PasswordPolicy.Check])
// and every read model go through it.
func (p PasswordPolicy) Effective() PasswordPolicy {
	if p.IsZero() {
		return DefaultPasswordPolicy()
	}
	return p
}

// Equal reports whether p and other are identical.
func (p PasswordPolicy) Equal(other PasswordPolicy) bool {
	return p == other
}

// Settings is the tenant-level operational settings composite.
// Currently wraps PasswordPolicy; extended with additional VOs as the
// domain grows (session timeout, MFA, IP allowlists).
type Settings struct {
	passwordPolicy PasswordPolicy
}

// NewSettings composes a Settings VO from the given password policy.
func NewSettings(p PasswordPolicy) Settings {
	return Settings{passwordPolicy: p}
}

// PasswordPolicy returns the tenant's password policy.
func (s Settings) PasswordPolicy() PasswordPolicy { return s.passwordPolicy }

// EffectivePasswordPolicy returns the policy that binds the tenant: the stored one,
// or the system default when none was saved. Readers (enforcement, the tenant read
// models, the capabilities rules) use this; only persistence uses the raw
// [Settings.PasswordPolicy], so that saving an unrelated attribute does not freeze
// today's default into the row.
func (s Settings) EffectivePasswordPolicy() PasswordPolicy { return s.passwordPolicy.Effective() }

// IsZero reports whether Settings is uninitialised.
func (s Settings) IsZero() bool { return s.passwordPolicy.IsZero() }

// Equal reports whether s and other are identical.
func (s Settings) Equal(other Settings) bool { return s.passwordPolicy.Equal(other.passwordPolicy) }

// MaxPasswordBytes bounds a password's size in bytes (1 KiB, roughly 256 typical
// characters; NIST SP 800-63B asks verifiers to accept at least 64). It is checked
// FIRST by [PasswordPolicy.Check], before any counting, and every command that
// sets a password calls Check before it hashes: a request body can be 1 MiB, and
// neither the class counting nor an Argon2id hash should run over that.
const MaxPasswordBytes = 1024

// ErrPasswordPolicy is the sentinel for a password that fails the policy.
var ErrPasswordPolicy = errors.New("tenant: password does not satisfy the password policy")

// Wire-stable rule codes surfaced in the 422 `errors` map (the web renders one
// message per code). Adding a code is additive; renaming one breaks clients.
const (
	PasswordRuleMinLength = "min_length"
	PasswordRuleMaxLength = "max_length"
	PasswordRuleUppercase = "requires_uppercase"
	PasswordRuleLowercase = "requires_lowercase"
	PasswordRuleDigit     = "requires_digit"
	PasswordRuleSymbol    = "requires_symbol"
)

// PasswordPolicyViolation lists every failed rule, in a fixed order. Its message
// carries rule codes only, never the password.
type PasswordPolicyViolation struct{ Rules []string }

// Error implements error.
func (v *PasswordPolicyViolation) Error() string {
	return ErrPasswordPolicy.Error() + ": " + strings.Join(v.Rules, ", ")
}

// Unwrap lets errors.Is(err, ErrPasswordPolicy) match.
func (v *PasswordPolicyViolation) Unwrap() error { return ErrPasswordPolicy }

// Check validates plaintext against the policy: size bound first, then length in
// runes, then the required classes by unicode category. A symbol is punctuation
// or a symbol (unicode.IsPunct or IsSymbol); whitespace is not one. A zero policy
// (a row that predates settings) is evaluated as [DefaultPasswordPolicy].
//
// Check does no I/O and never reads the stored hash, so it cannot tell a caller
// anything about the account: the answer is a function of the policy and the
// candidate alone.
func (p PasswordPolicy) Check(plaintext string) error {
	if len(plaintext) > MaxPasswordBytes {
		return &PasswordPolicyViolation{Rules: []string{PasswordRuleMaxLength}}
	}
	p = p.Effective()
	var upper, lower, digit, symbol bool
	for _, r := range plaintext {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digit = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			symbol = true
		}
	}
	var failed []string
	if utf8.RuneCountInString(plaintext) < p.minLength {
		failed = append(failed, PasswordRuleMinLength)
	}
	if p.requireUppercase && !upper {
		failed = append(failed, PasswordRuleUppercase)
	}
	if p.requireLowercase && !lower {
		failed = append(failed, PasswordRuleLowercase)
	}
	if p.requireDigit && !digit {
		failed = append(failed, PasswordRuleDigit)
	}
	if p.requireSymbol && !symbol {
		failed = append(failed, PasswordRuleSymbol)
	}
	if len(failed) > 0 {
		return &PasswordPolicyViolation{Rules: failed}
	}
	return nil
}
