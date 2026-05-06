package tenant

import (
	"fmt"
	"time"
)

// PasswordPolicy is a tenant's per-tenant password rules. Per
// `security.md` "PasswordPolicy" — overrides + tightens system
// defaults; never relaxes them.
//
// MinLength MUST be ≥ 8 (NIST SP 800-63B floor; system default).
// MaxFailedAttempts MUST be ≥ 3 (defeats accidental lockout but
// catches brute-force).
// LockoutMinutes is the cool-down after MaxFailedAttempts is reached.
type PasswordPolicy struct {
	minLength         int
	requireUppercase  bool
	requireLowercase  bool
	requireDigit      bool
	requireSymbol     bool
	maxFailedAttempts int
	lockoutMinutes    int
}

// PasswordPolicy floors per security.md. Tenants may set stricter
// values; relaxing below these returns ErrInvalid.
const (
	minPasswordLength       = 8
	minMaxFailedAttempts    = 3
	maxPasswordLength       = 128 // sanity cap on minLength choice
	maxMaxFailedAttempts    = 50  // sanity cap
	maxLockoutMinutes       = 24 * 60
)

// NewPasswordPolicy validates + returns a PasswordPolicy.
//
// minLength: 8-128 (NIST floor; system upper bound).
// maxFailedAttempts: 3-50.
// lockoutMinutes: 0-1440 (0 means no auto-unlock — admin-only).
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

// DefaultPasswordPolicy returns the system default — minLength=12,
// require-all-classes, 5 failed attempts, 15min lockout. Tenants may
// override via [NewPasswordPolicy].
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

// MinLength returns the minimum-password-length policy.
func (p PasswordPolicy) MinLength() int { return p.minLength }

// RequireUppercase reports whether at least one uppercase is required.
func (p PasswordPolicy) RequireUppercase() bool { return p.requireUppercase }

// RequireLowercase reports whether at least one lowercase is required.
func (p PasswordPolicy) RequireLowercase() bool { return p.requireLowercase }

// RequireDigit reports whether at least one digit is required.
func (p PasswordPolicy) RequireDigit() bool { return p.requireDigit }

// RequireSymbol reports whether at least one symbol is required.
func (p PasswordPolicy) RequireSymbol() bool { return p.requireSymbol }

// MaxFailedAttempts returns the lockout threshold.
func (p PasswordPolicy) MaxFailedAttempts() int { return p.maxFailedAttempts }

// LockoutMinutes returns the auto-unlock window. 0 = admin-only unlock.
func (p PasswordPolicy) LockoutMinutes() int { return p.lockoutMinutes }

// LockoutDuration returns LockoutMinutes as a time.Duration.
func (p PasswordPolicy) LockoutDuration() time.Duration {
	return time.Duration(p.lockoutMinutes) * time.Minute
}

// IsZero reports whether this is the empty zero value (vs a configured
// policy). Note: a configured policy with minLength=0 is rejected by
// NewPasswordPolicy, so zero value MEANS "uninitialised."
func (p PasswordPolicy) IsZero() bool { return p.minLength == 0 }

// Equal compares two PasswordPolicy by all fields.
func (p PasswordPolicy) Equal(other PasswordPolicy) bool {
	return p == other
}

// Settings is the tenant-level operational settings composite.
// Today: PasswordPolicy. Future fields (session timeout, MFA
// requirement, IP allowlists) will be added here as additional value
// objects per `audit-checklist.md` "VOs grow with the domain."
type Settings struct {
	passwordPolicy PasswordPolicy
}

// NewSettings composes a Settings VO. Pass [DefaultPasswordPolicy]
// for the system default.
func NewSettings(p PasswordPolicy) Settings {
	return Settings{passwordPolicy: p}
}

// PasswordPolicy returns the tenant's password policy.
func (s Settings) PasswordPolicy() PasswordPolicy { return s.passwordPolicy }

// IsZero reports whether settings is uninitialised.
func (s Settings) IsZero() bool { return s.passwordPolicy.IsZero() }

// Equal compares two Settings.
func (s Settings) Equal(other Settings) bool { return s.passwordPolicy.Equal(other.passwordPolicy) }
