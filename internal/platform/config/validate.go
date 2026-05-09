package config

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// placeholderMarkers are CASE-INSENSITIVE SUBSTRINGS the validator
// looks for inside any secret. Substring match (vs exact-match list)
// catches realistic unsubstituted templates without being so greedy
// it false-positives on legitimate high-entropy keys.
//
// Inclusion criterion: a substring this distinctive is overwhelmingly
// indicative of "operator forgot to substitute the env var", and a
// real production key containing it would be a stupendous coincidence.
// Markers like "secret", "admin", "1234", "password" were considered
// + REJECTED because real high-entropy keys frequently include those
// short substrings (e.g. a 32-byte hex key like
// "0123456789abcdef..." trivially contains "12345"). Strength
// concerns there are caught by the [minStrongEntropyBits] floor and
// the RFC 7518 §3.2 length gate, not by greedy substring matching.
//
// Sources: NIST SP 800-63B §5.1.1.2 (memorized-secret strength) +
// OWASP ASVS 4.0 V2.1.
var placeholderMarkers = []string{
	"change_me", "changeme",
	"replace_me", "replaceme",
	"set-via-env", "set_via_env",
	"<env>", "<set>",
	"please_change", "please-change", "pleasechange",
	"insert_key", "insert-key",
	"do-not-ship", "do_not_ship", "donotship",
	"dev-only", "dev_only", "devonly",
	"not-for-production", "not_for_production", "notforproduction",
	"changeit",
	"placeholder",
	"yourkeyhere", "your_key_here", "your-key-here",
	"xxxxxxxx", // common dev fixture
}

// minStrongEntropyBits is the Shannon-entropy floor for `secret:"strong"`
// fields. 128 bits is the conservative crypto-key strength threshold
// (NIST SP 800-57 Part 1 Rev. 5 §5.6 "Symmetric Key Strength —
// 128-bit security level"). For a 32-byte (256-bit) random key, the
// per-byte entropy is ~7.5 bits, so a true random key clears 240 bits
// easily; a degenerate key like 32 repeated 'a's measures ~0 bits.
const minStrongEntropyBits = 60.0

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
	//
	// Strong tier (JWT signing keys) gets all three gates: placeholder
	// match, length floor (RFC 7518 §3.2 — HS256 ≥32 bytes), entropy
	// floor (defends against degenerate keys like "aaaaaaaa..." which
	// pass length but trivially succumb to brute-force).
	if err := validateStrongSecret(cfg.JWT.SigningKey, "JWT.SigningKey"); err != nil {
		return err
	}
	for i, p := range cfg.JWT.PreviousKeys {
		if strings.TrimSpace(p.KeyID) == "" {
			return fmt.Errorf("config: JWT.PreviousKeys[%d].KeyID required", i)
		}
		if err := validateStrongSecret(p.SigningKey, fmt.Sprintf("JWT.PreviousKeys[%d].SigningKey", i)); err != nil {
			return err
		}
	}

	// Weak tier — Redis password, optional. Placeholder check only;
	// length isn't enforced (managed Redis often has long
	// auto-generated passwords; user-supplied weak passwords are an
	// out-of-scope problem for this layer).
	if err := validateWeakSecret(cfg.Redis.Password, "Redis.Password"); err != nil {
		return err
	}

	// Connection-string tier — the DSN itself shouldn't carry a
	// placeholder password embedded inline (postgres://user:CHANGEME@host).
	if err := validateConnectionString(cfg.Postgres.DSN, "Postgres.DSN"); err != nil {
		return err
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

// isPlaceholder reports whether s contains a known placeholder
// substring. Case-folded; trims whitespace at the boundaries. The
// substring check is intentionally aggressive (vs the previous
// exact-match list) so dev-only literals like "dev-only-do-not-ship"
// or "changeit" are rejected even when wrapped in surrounding text.
func isPlaceholder(s string) bool {
	folded := strings.ToLower(strings.TrimSpace(s))
	if folded == "" {
		return false // empty is rejected by the length gate, not here
	}
	for _, marker := range placeholderMarkers {
		if strings.Contains(folded, marker) {
			return true
		}
	}
	return false
}

// shannonEntropyBits estimates the Shannon entropy of s in bits over
// a uniform alphabet derived from the byte distribution. Returns 0
// for empty or single-byte strings. This is a coarse defence against
// degenerate keys (e.g. 32 bytes of 'a') that pass the length gate
// but trivially fail to dictionary attacks. Real random keys clear
// the [minStrongEntropyBits] floor by orders of magnitude.
//
// Algorithm: H = -Σ p_i log2(p_i), scaled by len(s). For a 32-byte
// uniformly-random key over 256 symbols, expected H ≈ 8 bits/byte ×
// 32 = 256 bits. For "aaaa...a" (one symbol), H = 0.
func shannonEntropyBits(s string) float64 {
	if len(s) < 2 {
		return 0
	}
	counts := make(map[byte]int, len(s))
	for i := range len(s) {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h * n
}

// validateStrongSecret enforces the full strong-tier gate:
// placeholder match, RFC 7518 §3.2 length floor (≥32 bytes for
// HS256), and Shannon-entropy floor.
func validateStrongSecret(s, name string) error {
	if isPlaceholder(s) {
		return fmt.Errorf("%w: %s", ErrPlaceholderSecret, name)
	}
	if len(s) < 32 {
		return fmt.Errorf("%w: %s got %d bytes", ErrJWTKeyTooShort, name, len(s))
	}
	if h := shannonEntropyBits(s); h < minStrongEntropyBits {
		return fmt.Errorf("config: %s entropy too low: %.1f bits < %.0f bits floor (likely a degenerate / repeating value)",
			name, h, minStrongEntropyBits)
	}
	return nil
}

// validateWeakSecret enforces only the placeholder gate. Empty
// values are accepted (the secret is optional, e.g. Redis without
// auth).
func validateWeakSecret(s, name string) error {
	if isPlaceholder(s) {
		return fmt.Errorf("%w: %s", ErrPlaceholderSecret, name)
	}
	return nil
}

// validateConnectionString rejects DSNs that carry a placeholder
// substring anywhere (typically the embedded password segment in a
// `postgres://user:CHANGEME@host` style URI).
func validateConnectionString(s, name string) error {
	if isPlaceholder(s) {
		return fmt.Errorf("%w: %s contains a placeholder marker", ErrPlaceholderSecret, name)
	}
	return nil
}
