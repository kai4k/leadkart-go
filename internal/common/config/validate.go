package config

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
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

	if err := validateHTTP(cfg); err != nil {
		return err
	}

	// OTel sanity.
	if cfg.OTel.SampleRatio < 0 || cfg.OTel.SampleRatio > 1 {
		return fmt.Errorf("config: OTel.SampleRatio must be in [0,1] (got %v)", cfg.OTel.SampleRatio)
	}

	return validateArgon2(cfg.Argon2)
}

// Bounds of the Argon2 gate: at least one slot, and not so many that the memory
// the gate exists to bound (slots x ~19.5 MiB) could reach the container limit.
const (
	maxArgon2Concurrent = 64
	minArgon2Queue      = 100 * time.Millisecond
	maxArgon2Queue      = 30 * time.Second
)

func validateArgon2(c Argon2Config) error {
	if c.MaxConcurrent < 1 || c.MaxConcurrent > maxArgon2Concurrent {
		return fmt.Errorf("config: Argon2.MaxConcurrent must be in [1,%d] (got %d)", maxArgon2Concurrent, c.MaxConcurrent)
	}
	if c.QueueTimeout < minArgon2Queue || c.QueueTimeout > maxArgon2Queue {
		return fmt.Errorf("config: Argon2.QueueTimeout must be in [%s,%s] (got %s)", minArgon2Queue, maxArgon2Queue, c.QueueTimeout)
	}
	return nil
}

// maxRouteBurst bounds a per-route bucket: a burst in the thousands is a limit
// in name only.
const maxRouteBurst = 1000

// validateHTTP gates the trusted-proxy list and the per-route rate limits.
//
// Outside production an empty list is allowed (trust nobody). In production the
// operator must decide: a list of the proxy addresses, or the word "none" for a
// directly exposed API. An unset value is a start-up error there, because
// behind a BFF it silently puts every user into one rate-limit bucket per
// credential route. A production list is also refused when it is wider than
// /8 (IPv4) or /32 (IPv6), or leaves the private / loopback / link-local /
// CGNAT / ULA ranges.
func validateHTTP(cfg AppConfig) error {
	trusted, err := httpmw.ParseTrustedProxies(cfg.HTTP.TrustedProxies)
	if err != nil {
		return fmt.Errorf("config: HTTP.TrustedProxies: %w", err)
	}
	if cfg.Env == "production" {
		if len(trusted) == 0 && !strings.EqualFold(strings.TrimSpace(cfg.HTTP.TrustedProxies), httpmw.NoTrustedProxies) {
			return errors.New("config: HTTP.TrustedProxies is required in production: set LEADKART_HTTP__TRUSTED_PROXIES to the address or CIDR of the BFF / ingress in front of the API, or to \"none\" if the API is directly exposed")
		}
		if err := httpmw.ValidateProductionTrustedProxies(trusted); err != nil {
			return fmt.Errorf("config: HTTP.TrustedProxies: %w", err)
		}
	}
	for _, r := range []struct {
		name  string
		limit RouteLimit
	}{
		{"login", cfg.RateLimit.Login},
		{"refresh", cfg.RateLimit.Refresh},
		{"password_reset_request", cfg.RateLimit.PasswordResetRequest},
		{"password_reset", cfg.RateLimit.PasswordReset},
		{"email_change_confirm", cfg.RateLimit.EmailChangeConfirm},
		{"tenant_registration", cfg.RateLimit.TenantRegistration},
		{"change_password", cfg.RateLimit.ChangePassword},
	} {
		// !(>0) also rejects NaN; IsInf rejects +Inf (a limit that never limits).
		if !(r.limit.PerMinute > 0) || math.IsInf(r.limit.PerMinute, 0) || r.limit.Burst < 1 || r.limit.Burst > maxRouteBurst {
			return fmt.Errorf("config: RateLimit.%s needs a finite PerMinute > 0 and 1 <= Burst <= %d (got %v, %d)",
				r.name, maxRouteBurst, r.limit.PerMinute, r.limit.Burst)
		}
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
