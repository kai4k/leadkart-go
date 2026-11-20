package config_test

import (
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/config"
)

const validJWTKey = "0123456789abcdef0123456789abcdef" // 32 bytes

func validBaseConfig() config.AppConfig {
	c := config.Defaults()
	c.Postgres.DSN = "postgres://leadkart_app:pw@localhost:5432/leadkart?sslmode=disable"
	c.Redis.Addr = "localhost:6379"
	c.JWT.KeyID = "k1"
	c.JWT.SigningKey = validJWTKey
	return c
}

func TestValidate_HappyPath(t *testing.T) {
	t.Parallel()
	if err := config.Validate(validBaseConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsMissingPostgres(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.Postgres.DSN = ""
	if err := config.Validate(c); !errors.Is(err, config.ErrMissingPostgres) {
		t.Fatalf("expected ErrMissingPostgres, got %v", err)
	}
}

func TestValidate_RejectsMissingRedis(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.Redis.Addr = ""
	if err := config.Validate(c); !errors.Is(err, config.ErrMissingRedis) {
		t.Fatalf("expected ErrMissingRedis, got %v", err)
	}
}

func TestValidate_RejectsMissingJWTKeyID(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.JWT.KeyID = ""
	if err := config.Validate(c); !errors.Is(err, config.ErrMissingJWTKeyID) {
		t.Fatalf("expected ErrMissingJWTKeyID, got %v", err)
	}
}

func TestValidate_RejectsShortJWTKey(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.JWT.SigningKey = "too-short"
	err := config.Validate(c)
	if !errors.Is(err, config.ErrJWTKeyTooShort) {
		t.Fatalf("expected ErrJWTKeyTooShort, got %v", err)
	}
}

func TestValidate_RejectsShortPreviousKey(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.JWT.PreviousKeys = []config.PreviousSigningKey{
		{KeyID: "k0", SigningKey: "tiny"},
	}
	err := config.Validate(c)
	if !errors.Is(err, config.ErrJWTKeyTooShort) {
		t.Fatalf("expected ErrJWTKeyTooShort on PreviousKey, got %v", err)
	}
}

func TestValidate_RejectsPlaceholderSecrets(t *testing.T) {
	t.Parallel()
	// Placeholder check runs BEFORE the length gate (intentional —
	// "you forgot to substitute the env var" beats "make it longer").
	// So values short OR long should both surface ErrPlaceholderSecret.
	cases := []string{
		"CHANGE_ME",
		"change_me",
		"REPLACE_ME",
		"<set-via-env>",
		"dev-jwt-signing-key-please-change-in-production",
		// Substring-match cases (post-A4 hardening): the marker can
		// appear anywhere inside the value, not just as the whole
		// string. These would have slipped through the previous
		// exact-match list.
		"prefix-CHANGE_ME-suffix",
		"my-do-not-ship-key-32-bytes-long-x",
		"dev-only-do-not-ship-this-32byte-secret-x", // the docker compose dev literal
		"yourkeyhere-padded-to-32-bytes-okx",
	}
	for _, val := range cases {
		val := val
		t.Run(val, func(t *testing.T) {
			t.Parallel()
			c := validBaseConfig()
			c.JWT.SigningKey = val
			err := config.Validate(c)
			if !errors.Is(err, config.ErrPlaceholderSecret) {
				t.Fatalf("expected ErrPlaceholderSecret for %q, got %v", val, err)
			}
		})
	}
}

func TestValidate_RejectsRefreshTTLInversion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*config.RefreshConfig)
	}{
		{"absolute < sliding", func(r *config.RefreshConfig) {
			r.AbsoluteTTL = time.Hour
			r.SlidingTTL = 24 * time.Hour
		}},
		{"absolute = sliding", func(r *config.RefreshConfig) {
			r.AbsoluteTTL = time.Hour
			r.SlidingTTL = time.Hour
		}},
		{"sliding < grace", func(r *config.RefreshConfig) {
			r.SlidingTTL = time.Second
			r.GraceWindow = time.Minute
		}},
		{"grace above the cap", func(r *config.RefreshConfig) {
			r.GraceWindow = config.MaxRefreshGraceWindow + time.Second
			r.SlidingTTL = time.Hour
		}},
		{"grace negative", func(r *config.RefreshConfig) {
			r.GraceWindow = -time.Second
		}},
		{"family cap zero", func(r *config.RefreshConfig) {
			r.FamilyCap = 0
		}},
		{"absolute zero", func(r *config.RefreshConfig) {
			r.AbsoluteTTL = 0
		}},
		{"sliding zero", func(r *config.RefreshConfig) {
			r.SlidingTTL = 0
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validBaseConfig()
			tc.mutate(&c.Refresh)
			if err := config.Validate(c); err == nil {
				t.Fatalf("%s: expected error", tc.name)
			}
		})
	}
}

func TestValidate_RejectsBadOTelSampleRatio(t *testing.T) {
	t.Parallel()
	for _, ratio := range []float64{-0.1, 1.1, 2.0, -1.0} {
		ratio := ratio
		t.Run("", func(t *testing.T) {
			t.Parallel()
			c := validBaseConfig()
			c.OTel.SampleRatio = ratio
			if err := config.Validate(c); err == nil {
				t.Fatalf("ratio=%v: expected error", ratio)
			}
		})
	}
}

func TestDefaults_PassValidationWithRequiredFieldsAdded(t *testing.T) {
	t.Parallel()
	c := config.Defaults()
	c.Postgres.DSN = "postgres://x:y@localhost/z"
	c.Redis.Addr = "localhost:6379"
	c.JWT.KeyID = "k1"
	c.JWT.SigningKey = validJWTKey
	if err := config.Validate(c); err != nil {
		t.Fatalf("Defaults + required scalars should validate: %v", err)
	}
}

// TestValidate_RejectsLowEntropyStrongSecret pins the entropy floor
// added in the secrets-validator hardening. A 32-byte key of 'a'
// passes the RFC 7518 §3.2 length gate but trivially loses to a
// dictionary attack — the Shannon-entropy check catches it.
func TestValidate_RejectsLowEntropyStrongSecret(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	// 32 bytes, entropy = 0 (single symbol).
	c.JWT.SigningKey = strings.Repeat("a", 32)
	err := config.Validate(c)
	if err == nil {
		t.Fatal("expected entropy-floor error, got nil")
	}
	if !strings.Contains(err.Error(), "entropy too low") {
		t.Fatalf("expected entropy error, got: %v", err)
	}
}

// TestValidate_RejectsPlaceholderInPostgresDSN pins the
// connection-string-tier scan: a DSN with a placeholder embedded
// in the password segment fails boot.
func TestValidate_RejectsPlaceholderInPostgresDSN(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.Postgres.DSN = "postgres://user:CHANGE_ME@localhost:5432/leadkart"
	err := config.Validate(c)
	if !errors.Is(err, config.ErrPlaceholderSecret) {
		t.Fatalf("expected ErrPlaceholderSecret on DSN, got %v", err)
	}
}

// TestValidate_RejectsPlaceholderInRedisPassword pins the weak-tier
// scan.
func TestValidate_RejectsPlaceholderInRedisPassword(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.Redis.Password = "redis-changeme-please"
	err := config.Validate(c)
	if !errors.Is(err, config.ErrPlaceholderSecret) {
		t.Fatalf("expected ErrPlaceholderSecret on Redis.Password, got %v", err)
	}
}

// TestValidate_AcceptsHighEntropyJWTKey confirms the entropy floor
// doesn't false-positive on a typical 32-byte hex key (16-symbol
// alphabet, ~128 bits of entropy — well above the 60-bit floor).
func TestValidate_AcceptsHighEntropyJWTKey(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.JWT.SigningKey = "0123456789abcdef0123456789abcdef" // 128 bits
	if err := config.Validate(c); err != nil {
		t.Fatalf("hex 32-byte key should pass: %v", err)
	}
}

func TestValidate_TrustedProxies(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "10.0.0.0/8, 127.0.0.1, ::1"} {
		c := validBaseConfig()
		c.HTTP.TrustedProxies = in
		if err := config.Validate(c); err != nil {
			t.Errorf("TrustedProxies=%q: %v", in, err)
		}
	}
	for _, in := range []string{"10.0.0.0/33", "not-an-ip", "0.0.0.0/0"} {
		c := validBaseConfig()
		c.HTTP.TrustedProxies = in
		err := config.Validate(c)
		if err == nil || !strings.Contains(err.Error(), "HTTP.TrustedProxies") {
			t.Errorf("TrustedProxies=%q: err = %v, want a TrustedProxies error", in, err)
		}
	}
}

func TestValidate_RateLimitRoutesNeedPositiveRateAndBurst(t *testing.T) {
	t.Parallel()
	set := map[string]func(*config.RateLimitConfig, config.RouteLimit){
		"login":                  func(r *config.RateLimitConfig, l config.RouteLimit) { r.Login = l },
		"refresh":                func(r *config.RateLimitConfig, l config.RouteLimit) { r.Refresh = l },
		"password_reset_request": func(r *config.RateLimitConfig, l config.RouteLimit) { r.PasswordResetRequest = l },
		"password_reset":         func(r *config.RateLimitConfig, l config.RouteLimit) { r.PasswordReset = l },
		"email_change_confirm":   func(r *config.RateLimitConfig, l config.RouteLimit) { r.EmailChangeConfirm = l },
		"tenant_registration":    func(r *config.RateLimitConfig, l config.RouteLimit) { r.TenantRegistration = l },
	}
	for name, apply := range set {
		for _, bad := range []config.RouteLimit{{PerMinute: 0, Burst: 5}, {PerMinute: 5, Burst: 0}, {PerMinute: -1, Burst: 5}} {
			c := validBaseConfig()
			apply(&c.RateLimit, bad)
			err := config.Validate(c)
			if err == nil || !strings.Contains(err.Error(), "RateLimit."+name) {
				t.Errorf("%s %+v: err = %v, want a RateLimit.%s error", name, bad, err, name)
			}
		}
	}
}

func TestValidate_ProductionRequiresATrustedProxyDecision(t *testing.T) {
	t.Parallel()
	prod := func(in string) error {
		c := validBaseConfig()
		c.Env = "production"
		c.HTTP.TrustedProxies = in
		c.Identity.LockoutKey = validLockoutKey
		return config.Validate(c)
	}
	for _, in := range []string{"", "  ", ","} {
		if err := prod(in); err == nil || !strings.Contains(err.Error(), "required in production") {
			t.Errorf("production TrustedProxies=%q: err = %v, want the required-in-production error", in, err)
		}
	}
	for _, in := range []string{"none", "172.16.0.0/12", "10.0.0.0/8, 127.0.0.1, ::1", "fd12:3456::/32"} {
		if err := prod(in); err != nil {
			t.Errorf("production TrustedProxies=%q: %v", in, err)
		}
	}
	for _, in := range []string{"8.8.8.8", "203.0.113.0/24", "10.0.0.0/7", "2001:db8::/32", "fd00::/16", "172.0.0.0/8"} {
		if err := prod(in); err == nil || !strings.Contains(err.Error(), "HTTP.TrustedProxies") {
			t.Errorf("production TrustedProxies=%q: err = %v, want a TrustedProxies error", in, err)
		}
	}
	// Outside production the wide dev-runbook range and the empty list stay valid.
	for _, env := range []string{"dev", "staging"} {
		c := validBaseConfig()
		c.Env = env
		for _, in := range []string{"", "172.16.0.0/12,192.168.0.0/16,127.0.0.1/32,::1/128", "8.8.8.8"} {
			c.HTTP.TrustedProxies = in
			if err := config.Validate(c); err != nil {
				t.Errorf("%s TrustedProxies=%q: %v", env, in, err)
			}
		}
	}
}

func TestValidate_RateLimitRejectsNonFiniteRatesAndHugeBursts(t *testing.T) {
	t.Parallel()
	for _, bad := range []config.RouteLimit{
		{PerMinute: math.Inf(1), Burst: 5},
		{PerMinute: math.NaN(), Burst: 5},
		{PerMinute: 5, Burst: 1001},
		{PerMinute: 5, Burst: 1 << 30},
	} {
		c := validBaseConfig()
		c.RateLimit.Login = bad
		if err := config.Validate(c); err == nil || !strings.Contains(err.Error(), "RateLimit.login") {
			t.Errorf("%+v: err = %v, want a RateLimit.login error", bad, err)
		}
	}
	c := validBaseConfig()
	c.RateLimit.Login = config.RouteLimit{PerMinute: 30, Burst: 1000}
	if err := config.Validate(c); err != nil {
		t.Errorf("Burst 1000 is the ceiling and must pass: %v", err)
	}
}

func TestValidate_Argon2Bounds(t *testing.T) {
	t.Parallel()
	for _, bad := range []config.Argon2Config{
		{MaxConcurrent: 0, QueueTimeout: time.Second, MaxPerClient: 1},
		{MaxConcurrent: -1, QueueTimeout: time.Second, MaxPerClient: 1},
		{MaxConcurrent: 65, QueueTimeout: time.Second, MaxPerClient: 1},
		{MaxConcurrent: 16, QueueTimeout: 0, MaxPerClient: 2},
		{MaxConcurrent: 16, QueueTimeout: 99 * time.Millisecond, MaxPerClient: 2},
		{MaxConcurrent: 16, QueueTimeout: 31 * time.Second, MaxPerClient: 2},
		// Fix round 12: the per-client cap and the reserved share.
		{MaxConcurrent: 16, QueueTimeout: time.Second, MaxPerClient: 0},
		{MaxConcurrent: 16, QueueTimeout: time.Second, MaxPerClient: 17},
		{MaxConcurrent: 16, QueueTimeout: time.Second, MaxPerClient: 2, ReservedAuthenticated: -1},
		{MaxConcurrent: 16, QueueTimeout: time.Second, MaxPerClient: 2, ReservedAuthenticated: 16},
		{MaxConcurrent: 1, QueueTimeout: time.Second, MaxPerClient: 1, ReservedAuthenticated: 1},
	} {
		c := validBaseConfig()
		c.Argon2 = bad
		if err := config.Validate(c); err == nil || !strings.Contains(err.Error(), "Argon2.") {
			t.Errorf("%+v: err = %v, want an Argon2 error", bad, err)
		}
	}
	for _, ok := range []config.Argon2Config{
		{MaxConcurrent: 1, QueueTimeout: 100 * time.Millisecond, MaxPerClient: 1},
		{MaxConcurrent: 64, QueueTimeout: 30 * time.Second, MaxPerClient: 64, ReservedAuthenticated: 63},
		{MaxConcurrent: 16, QueueTimeout: time.Second, MaxPerClient: 2, ReservedAuthenticated: 0},
	} {
		c := validBaseConfig()
		c.Argon2 = ok
		if err := config.Validate(c); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}

func TestValidate_ChangePasswordLimitIsValidatedLikeTheRouteLimits(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.RateLimit.ChangePassword = config.RouteLimit{PerMinute: 0, Burst: 5}
	if err := config.Validate(c); err == nil || !strings.Contains(err.Error(), "RateLimit.change_password") {
		t.Fatalf("err = %v", err)
	}
}

// A random 32-byte key, base64.
const validLockoutKey = "GuFuceASeYJI/9HY9E67i3iDbGag7KAH9bmR2CKvObQ="

func TestValidate_LockoutKey(t *testing.T) {
	t.Parallel()
	base := func() config.AppConfig {
		c := validBaseConfig()
		c.Env = "production"
		c.HTTP.TrustedProxies = "none"
		c.Identity.LockoutKey = validLockoutKey
		return c
	}
	if err := config.Validate(base()); err != nil {
		t.Fatalf("a valid production config: %v", err)
	}
	c := base()
	c.Identity.LockoutKey = ""
	if err := config.Validate(c); !errors.Is(err, config.ErrMissingLockoutKey) {
		t.Errorf("production without a lockout key: %v, want ErrMissingLockoutKey", err)
	}
	for name, bad := range map[string]string{
		"not base64":     "!!!not-base64-at-all!!!",
		"too short":      "c2hvcnQ=",
		"placeholder":    "CHANGE_ME_CHANGE_ME_CHANGE_ME_CHANGE_ME_CHANGE_ME",
		"low entropy":    "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=",
		"the signing key": validJWTKey,
	} {
		c := base()
		c.Identity.LockoutKey = bad
		if err := config.Validate(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Outside production an empty key is allowed (the caller falls back and warns).
	dev := validBaseConfig()
	if err := config.Validate(dev); err != nil {
		t.Errorf("dev without a lockout key: %v", err)
	}
}

// The worker has no listener, limits or password work: it validates what it uses.
func TestValidateWorker_DoesNotDemandTheAPIsSettings(t *testing.T) {
	t.Parallel()
	c := validBaseConfig()
	c.Env = "production"
	c.HTTP.TrustedProxies = "" // the API would refuse this in production
	c.Identity.LockoutKey = ""
	c.Argon2 = config.Argon2Config{}
	if err := config.ValidateWorker(c); err != nil {
		t.Fatalf("a production worker without the API's settings: %v", err)
	}
	if err := config.Validate(c); err == nil {
		t.Fatal("the API accepted the same config")
	}
	c.Postgres.DSN = ""
	if err := config.ValidateWorker(c); !errors.Is(err, config.ErrMissingPostgres) {
		t.Fatalf("the worker must still require the database: %v", err)
	}
}

// The cap itself and the strict rotation (0) are valid.
func TestValidate_AcceptsGraceAtTheCapAndZero(t *testing.T) {
	t.Parallel()
	for _, g := range []time.Duration{0, time.Second, config.MaxRefreshGraceWindow} {
		c := validBaseConfig()
		c.Refresh.SlidingTTL = time.Hour
		c.Refresh.GraceWindow = g
		if err := config.Validate(c); err != nil {
			t.Errorf("grace %s: %v", g, err)
		}
	}
}

// Fix round 13, item 10: the lockout key must differ from the JWT key as BYTES, not
// as strings: base64 of the signing key's bytes (or another encoding of the same
// bytes) is the same secret.
func TestValidate_LockoutKeyIsNotTheJWTKeyInAnotherEncoding(t *testing.T) {
	t.Parallel()
	const strongJWT = "Xk9dQ2mZp7LwR4vTb8NcYeHs1AfJu6Gq" // 32 bytes used as-is by the issuer
	prod := func() config.AppConfig {
		c := validBaseConfig()
		c.Env = "production"
		c.HTTP.TrustedProxies = "none"
		return c
	}
	t.Run("base64 of the signing key's bytes", func(t *testing.T) {
		t.Parallel()
		c := prod()
		c.JWT.SigningKey = strongJWT
		c.Identity.LockoutKey = base64.StdEncoding.EncodeToString([]byte(strongJWT))
		err := config.Validate(c)
		if err == nil || !strings.Contains(err.Error(), "differ from JWT.SigningKey") {
			t.Fatalf("lockout key = base64(JWT key): %v, want the must-differ error", err)
		}
	})
	t.Run("the JWT key is itself base64 and the lockout key is the same bytes in another alphabet", func(t *testing.T) {
		t.Parallel()
		raw, err := base64.StdEncoding.DecodeString(validLockoutKey)
		if err != nil {
			t.Fatal(err)
		}
		c := prod()
		c.JWT.SigningKey = validLockoutKey // the std, padded encoding
		c.Identity.LockoutKey = base64.RawURLEncoding.EncodeToString(raw)
		if c.Identity.LockoutKey == c.JWT.SigningKey {
			t.Fatal("setup: the two strings must differ")
		}
		err = config.Validate(c)
		if err == nil || !strings.Contains(err.Error(), "differ from JWT.SigningKey") {
			t.Fatalf("same decoded bytes: %v, want the must-differ error", err)
		}
	})
	t.Run("an unrelated key is fine", func(t *testing.T) {
		t.Parallel()
		c := prod()
		c.JWT.SigningKey = strongJWT
		c.Identity.LockoutKey = validLockoutKey
		if err := config.Validate(c); err != nil {
			t.Fatalf("independent keys: %v", err)
		}
	})
}
