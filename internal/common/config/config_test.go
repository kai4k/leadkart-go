package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/config"
)

// setEnv applies a map of LEADKART_* env vars for the duration of one
// test. t.Setenv reverts on completion + isolates per-test state.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// minRequired sets the four mandatory env vars Validate enforces; any
// test whose subject isn't a missing-field path needs these set.
func minRequired() map[string]string {
	return map[string]string{
		"LEADKART_POSTGRES__DSN":    "postgres://leadkart_app:pw@localhost:5432/leadkart?sslmode=disable",
		"LEADKART_REDIS__ADDR":      "localhost:6379",
		"LEADKART_JWT__KEY_ID":      "k1",
		"LEADKART_JWT__SIGNING_KEY": validJWTKey,
	}
}

// Owner ruling 2026-11-21: "a session unused for 24 hours ends" (idle timeout), the
// absolute session length stays 14 days, and the default still passes validation
// (SlidingTTL must exceed GraceWindow and stay below AbsoluteTTL).
//
// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_RefreshLifetimeDefaults(t *testing.T) {
	setEnv(t, minRequired())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Refresh.SlidingTTL != 24*time.Hour {
		t.Fatalf("default Refresh.SlidingTTL (idle timeout) = %v, want 24h", cfg.Refresh.SlidingTTL)
	}
	if cfg.Refresh.AbsoluteTTL != 14*24*time.Hour {
		t.Fatalf("default Refresh.AbsoluteTTL = %v, want 336h (14 days)", cfg.Refresh.AbsoluteTTL)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_DefaultsApplied(t *testing.T) {
	setEnv(t, minRequired())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen.API != ":8080" {
		t.Fatalf("default Listen.API: got %q want :8080", cfg.Listen.API)
	}
	if cfg.Listen.Admin != ":9090" {
		t.Fatalf("default Listen.Admin: got %q want :9090", cfg.Listen.Admin)
	}
	if cfg.Refresh.FamilyCap != 5 {
		t.Fatalf("default Refresh.FamilyCap: got %d want 5", cfg.Refresh.FamilyCap)
	}
	if cfg.OTel.ServiceName != "leadkart-api" {
		t.Fatalf("default OTel.ServiceName: got %q", cfg.OTel.ServiceName)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_EnvOverridesDefault(t *testing.T) {
	setEnv(t, minRequired())
	t.Setenv("LEADKART_LISTEN__API", ":9000")
	t.Setenv("LEADKART_OTEL__SERVICE_NAME", "leadkart-test")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen.API != ":9000" {
		t.Fatalf("env override Listen.API: got %q want :9000", cfg.Listen.API)
	}
	if cfg.OTel.ServiceName != "leadkart-test" {
		t.Fatalf("env override OTel.ServiceName: got %q want leadkart-test", cfg.OTel.ServiceName)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_FileThenEnv(t *testing.T) {
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "leadkart.yaml")
	yaml := `
env: production
http:
  trusted_proxies: none
listen:
  api: ":7000"
otel:
  service_name: leadkart-from-file
  sample_ratio: 0.25
`
	writeFile(t, yamlFile, yaml)

	setEnv(t, minRequired())
	t.Setenv("LEADKART_CONFIG_FILE", yamlFile)
	t.Setenv("LEADKART_IDENTITY__LOCKOUT_KEY", "GuFuceASeYJI/9HY9E67i3iDbGag7KAH9bmR2CKvObQ=")
	t.Setenv("LEADKART_LISTEN__API", ":7777") // env should win over file

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Env != "production" {
		t.Fatalf("file Env: got %q want production", cfg.Env)
	}
	if cfg.Listen.API != ":7777" {
		t.Fatalf("env beats file: got %q want :7777", cfg.Listen.API)
	}
	if cfg.OTel.ServiceName != "leadkart-from-file" {
		t.Fatalf("file OTel.ServiceName: got %q", cfg.OTel.ServiceName)
	}
	if cfg.OTel.SampleRatio != 0.25 {
		t.Fatalf("file OTel.SampleRatio: got %v want 0.25", cfg.OTel.SampleRatio)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_RejectsMissingPostgres(t *testing.T) {
	required := minRequired()
	delete(required, "LEADKART_POSTGRES__DSN")
	setEnv(t, required)
	_, err := config.Load()
	if !errors.Is(err, config.ErrMissingPostgres) {
		t.Fatalf("expected ErrMissingPostgres, got %v", err)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_RejectsShortJWTKey(t *testing.T) {
	required := minRequired()
	required["LEADKART_JWT__SIGNING_KEY"] = "too-short"
	setEnv(t, required)
	_, err := config.Load()
	if !errors.Is(err, config.ErrJWTKeyTooShort) {
		t.Fatalf("expected ErrJWTKeyTooShort, got %v", err)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_RejectsBadConfigFile(t *testing.T) {
	setEnv(t, minRequired())
	t.Setenv("LEADKART_CONFIG_FILE", "/no/such/file.yaml")
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_HTTPAndRateLimitDefaults(t *testing.T) {
	setEnv(t, minRequired())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.TrustedProxies != "" {
		t.Fatalf("TrustedProxies default %q, want empty (trust nobody)", cfg.HTTP.TrustedProxies)
	}
	want := config.RateLimitConfig{
		Login:                config.RouteLimit{PerMinute: 30, Burst: 10},
		Refresh:              config.RouteLimit{PerMinute: 120, Burst: 30},
		PasswordResetRequest: config.RouteLimit{PerMinute: 5, Burst: 5},
		PasswordReset:        config.RouteLimit{PerMinute: 10, Burst: 5},
		EmailChangeConfirm:   config.RouteLimit{PerMinute: 10, Burst: 5},
		TenantRegistration:   config.RouteLimit{PerMinute: 10, Burst: 5},
		ChangePassword:       config.RouteLimit{PerMinute: 5, Burst: 5},
	}
	if cfg.RateLimit != want {
		t.Fatalf("RateLimit defaults = %+v, want %+v", cfg.RateLimit, want)
	}
	if cfg.Argon2 != (config.Argon2Config{MaxConcurrent: 16, QueueTimeout: 2 * time.Second, MaxPerClient: 2, ReservedAuthenticated: 4}) {
		t.Fatalf("Argon2 defaults = %+v", cfg.Argon2)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_Argon2AndChangePasswordEnvOverrides(t *testing.T) {
	setEnv(t, minRequired())
	t.Setenv("LEADKART_ARGON2__MAX_CONCURRENT", "8")
	t.Setenv("LEADKART_ARGON2__QUEUE_TIMEOUT", "500ms")
	t.Setenv("LEADKART_RATE_LIMIT__CHANGE_PASSWORD__PER_MINUTE", "2")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Argon2.MaxConcurrent != 8 || cfg.Argon2.QueueTimeout != 500*time.Millisecond {
		t.Fatalf("Argon2 = %+v", cfg.Argon2)
	}
	if got := cfg.RateLimit.ChangePassword; got.PerMinute != 2 || got.Burst != 5 {
		t.Fatalf("ChangePassword = %+v, want {2 5}", got)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_HTTPAndRateLimitEnvOverrides(t *testing.T) {
	setEnv(t, minRequired())
	t.Setenv("LEADKART_HTTP__TRUSTED_PROXIES", "10.0.0.0/8")
	t.Setenv("LEADKART_RATE_LIMIT__LOGIN__PER_MINUTE", "600")
	t.Setenv("LEADKART_RATE_LIMIT__LOGIN__BURST", "50")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.TrustedProxies != "10.0.0.0/8" {
		t.Fatalf("TrustedProxies = %q", cfg.HTTP.TrustedProxies)
	}
	if got := cfg.RateLimit.Login; got.PerMinute != 600 || got.Burst != 50 {
		t.Fatalf("Login = %+v, want {600 50}", got)
	}
	if cfg.RateLimit.Refresh.PerMinute != 120 {
		t.Fatalf("an untouched route lost its default: %+v", cfg.RateLimit.Refresh)
	}
}

// arch-test:serial — uses t.Setenv (incompatible with t.Parallel)
func TestLoad_RejectsBadTrustedProxies(t *testing.T) {
	setEnv(t, minRequired())
	t.Setenv("LEADKART_HTTP__TRUSTED_PROXIES", "10.0.0.0/33")
	if _, err := config.Load(); err == nil {
		t.Fatal("Load accepted an invalid CIDR")
	}
}
