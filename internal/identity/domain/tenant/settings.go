package tenant

import (
	"fmt"
	"time"
)

// PasswordPolicy defines per-tenant password rules (security.md). Values may
// tighten but never relax system defaults: MinLength ≥ 8 (NIST SP 800-63B),
// MaxFailedAttempts ≥ 3, LockoutMinutes ≥ 0.
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

// DefaultPasswordPolicy returns the system default: minLength=12,
// all character classes required, 5 attempts, 15 min lockout.
func DefaultPasswordPolicy() PasswordPolicy {
	return PasswordPolicy{
		minLength:         12,
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

// IsZero reports whether Settings is uninitialised.
func (s Settings) IsZero() bool { return s.passwordPolicy.IsZero() }

// Equal reports whether s and other are identical.
func (s Settings) Equal(other Settings) bool { return s.passwordPolicy.Equal(other.passwordPolicy) }
