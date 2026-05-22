package config_test

import (
	"errors"
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
