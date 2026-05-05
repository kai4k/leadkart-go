package config

import (
	"errors"
	"fmt"
	"strings"
)

// placeholderSecrets are values the secrets validator REFUSES at boot.
// Sources: dev `appsettings.Development.json` literal values, README
// templates, common `.env.example` placeholders. Add new strings only
// after confirming they cannot legitimately appear in production.
var placeholderSecrets = []string{
	"CHANGE_ME",
	"CHANGEME",
	"REPLACE_ME",
	"REPLACEME",
	"<set-via-env>",
	"<set_via_env>",
	"PLEASE_CHANGE",
	"INSERT_KEY_HERE",
	"dev-jwt-signing-key-please-change-in-production",
	"dev-jwt-key-not-for-production-use",
}

// Validate runs every gate the loader applies after merging providers.
//
// Gates:
//
//   - required scalars (Postgres.DSN, Redis.Addr, JWT.KeyID).
//   - JWT.SigningKey + each PreviousKey.SigningKey ≥ 32 bytes.
//   - No `secret:"strong"` field carries a placeholder.
//   - Refresh TTL sanity (absolute > sliding > grace; family_cap ≥ 1).
//
// Production fails strict on every gate. Dev relaxes nothing — same
// rules everywhere so contributors don't bisect "works on my machine".
func Validate(cfg AppConfig) error {
	if strings.TrimSpace(cfg.Postgres.DSN) == "" {
		return ErrMissingPostgres
	}
	if strings.TrimSpace(cfg.Redis.Addr) == "" {
		return ErrMissingRedis
	}
	if strings.TrimSpace(cfg.JWT.KeyID) == "" {
		return ErrMissingJWTKeyID
	}

	// Placeholder check runs BEFORE the length gate: a placeholder is
	// more actionable as an error than "key too short" — operator sees
	// "you forgot to substitute the env var" rather than "configure
	// a longer key" (and then they configure CHANGE_MEEEEEEEE...).
	if isPlaceholder(cfg.JWT.SigningKey) {
		return fmt.Errorf("%w: JWT.SigningKey", ErrPlaceholderSecret)
	}
	for i, p := range cfg.JWT.PreviousKeys {
		if isPlaceholder(p.SigningKey) {
			return fmt.Errorf("%w: JWT.PreviousKeys[%d].SigningKey", ErrPlaceholderSecret, i)
		}
	}

	if len(cfg.JWT.SigningKey) < 32 {
		return fmt.Errorf("%w: got %d bytes", ErrJWTKeyTooShort, len(cfg.JWT.SigningKey))
	}
	for i, p := range cfg.JWT.PreviousKeys {
		if strings.TrimSpace(p.KeyID) == "" {
			return fmt.Errorf("config: JWT.PreviousKeys[%d].KeyID required", i)
		}
		if len(p.SigningKey) < 32 {
			return fmt.Errorf("%w: PreviousKeys[%d] got %d bytes", ErrJWTKeyTooShort, i, len(p.SigningKey))
		}
	}

	// Refresh TTL sanity.
	if cfg.Refresh.AbsoluteTTL <= 0 {
		return errors.New("config: Refresh.AbsoluteTTL must be positive")
	}
	if cfg.Refresh.SlidingTTL <= 0 {
		return errors.New("config: Refresh.SlidingTTL must be positive")
	}
	if cfg.Refresh.AbsoluteTTL <= cfg.Refresh.SlidingTTL {
		return errors.New("config: Refresh.AbsoluteTTL must exceed Refresh.SlidingTTL")
	}
	if cfg.Refresh.SlidingTTL <= cfg.Refresh.GraceWindow {
		return errors.New("config: Refresh.SlidingTTL must exceed Refresh.GraceWindow")
	}
	if cfg.Refresh.FamilyCap < 1 {
		return errors.New("config: Refresh.FamilyCap must be ≥1")
	}

	// OTel sanity.
	if cfg.OTel.SampleRatio < 0 || cfg.OTel.SampleRatio > 1 {
		return fmt.Errorf("config: OTel.SampleRatio must be in [0,1] (got %v)", cfg.OTel.SampleRatio)
	}

	return nil
}

// isPlaceholder reports whether s matches a known placeholder. Case
// folded so "change_me" + "Change_Me" both match.
func isPlaceholder(s string) bool {
	folded := strings.ToLower(strings.TrimSpace(s))
	if folded == "" {
		return false // empty is rejected by the length gate, not here
	}
	for _, p := range placeholderSecrets {
		if folded == strings.ToLower(p) {
			return true
		}
	}
	return false
}
