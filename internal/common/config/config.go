// Package config holds the strongly-typed [AppConfig] + the koanf-backed
// loader. Per ADR 0017: koanf v2 (Kailash Nadh) — composable env + file
// providers, no global state, generics-ready.
//
// Config flow:
//
//	Defaults → optional file (YAML) → env vars (LEADKART_*) → validation
//
// Env vars use the prefix `LEADKART_` and a `__` (double-underscore)
// nesting separator. e.g. `LEADKART_POSTGRES__DSN` → `Postgres.DSN`.
//
// File path is read from `LEADKART_CONFIG_FILE` (optional). Production
// typically passes a YAML; tests pass nothing and let env override.
//
// Citations: koanf README + Plausible Analytics + Brave Browser Go
// services + FerretDB use koanf v2 in production.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	envprovider "github.com/knadh/koanf/providers/env/v2"
	fileprovider "github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix is the env-var namespace LeadKart claims. Values without
// this prefix are ignored.
const EnvPrefix = "LEADKART_"

// EnvSeparator is the nesting separator inside env-var keys.
// `LEADKART_POSTGRES__DSN` → `Postgres.DSN`.
const EnvSeparator = "__"

// configFileEnvVar names the optional YAML file that supplies defaults
// + non-secret values; secrets always come from env.
const configFileEnvVar = "LEADKART_CONFIG_FILE"

// AppConfig is the strongly-typed runtime configuration.
//
// Field tags drive koanf binding (`koanf:"…"`) + secrets-validator
// behaviour (`secret:"…"`). The validator inspects `secret` tag values
// to apply byte-length floors + placeholder rejection per [security.md]
// "Secrets integrity — startup validator".
type AppConfig struct {
	Env      string         `koanf:"env"` // "dev" | "staging" | "production"
	Listen   ListenConfig   `koanf:"listen"`
	Postgres PostgresConfig `koanf:"postgres"`
	Redis    RedisConfig    `koanf:"redis"`
	JWT      JWTConfig      `koanf:"jwt"`
	Refresh  RefreshConfig  `koanf:"refresh"`
	OTel     OTelConfig     `koanf:"otel"`

	HTTP      HTTPConfig      `koanf:"http"`
	RateLimit RateLimitConfig `koanf:"rate_limit"`
	Argon2    Argon2Config    `koanf:"argon2"`
	Identity  IdentityConfig  `koanf:"identity"`
}

// IdentityConfig holds the Identity module's own secrets.
type IdentityConfig struct {
	// LockoutKey keys the failed-login counter of addresses with no usable account
	// (the phantom lockout, ADR 0068 D12): the table stores HMAC(key, email), never an
	// email. A dedicated secret, base64 (or hex) of at least 32 random bytes, so that
	// routine JWT signing-key rotation cannot reset every phantom counter while real
	// accounts stay locked (a short 423-vs-401 oracle after each rotation). REQUIRED
	// when Env is "production"; elsewhere an empty value falls back to a key derived
	// from the JWT signing key, with a boot WARN. Rotating it restarts the phantom
	// counters (they live 15 minutes) and has the same short oracle window, so rotate
	// it deliberately, not routinely. Generate with `openssl rand -base64 32`.
	LockoutKey string `koanf:"lockout_key" secret:"strong"`
}

// LockoutKeyBytes decodes LockoutKey (standard or URL base64, padded or not; a hex
// string decodes as base64 too). Empty returns nil, nil: the caller falls back.
func (i IdentityConfig) LockoutKeyBytes() ([]byte, error) {
	s := strings.TrimSpace(i.LockoutKey)
	if s == "" {
		return nil, nil
	}
	if b, ok := decodeBase64Any(s); ok {
		return b, nil
	}
	return nil, errors.New("config: Identity.LockoutKey is not base64 (generate one with `openssl rand -base64 32`)")
}

// decodeBase64Any decodes s as standard or URL base64, padded or not.
func decodeBase64Any(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// ListenConfig holds network bind addresses for the API + admin
// (pprof + metrics) listeners + the cmd/worker admin probe listener.
// Admin is a separate port so probes + diagnostics don't share the
// public listener (per [observability] doctrine — pprof never on the
// public port). WorkerAdmin is the cmd/worker process's equivalent —
// the worker exposes no public API, only its admin probes.
type ListenConfig struct {
	API         string `koanf:"api"`          // ":8080"
	Admin       string `koanf:"admin"`        // ":9090"
	WorkerAdmin string `koanf:"worker_admin"` // ":9091"
}

// PostgresConfig carries the leadkart_app role DSN. Migrations run as
// the owner role through cmd/migrate, not via this DSN.
type PostgresConfig struct {
	DSN string `koanf:"dsn" secret:"connection_string"`
}

// RedisConfig — go-redis/v9 connection. Required: HybridCache L2 +
// JWT blacklist + (future) ITicketStore + idempotency store all share
// this client per ADR 0015 (revised).
type RedisConfig struct {
	Addr     string `koanf:"addr"`                   // "localhost:6379"
	Password string `koanf:"password" secret:"weak"` // optional
	DB       int    `koanf:"db"`                     // 0-15
}

// JWTConfig — HS256 signing key + kid header per [security.md] "JWT
// signing key + key history". Production rotates by populating
// `PreviousKeys` for the access-token-lifetime × 2 grace window.
type JWTConfig struct {
	KeyID        string               `koanf:"key_id"`
	SigningKey   string               `koanf:"signing_key" secret:"strong"`
	PreviousKeys []PreviousSigningKey `koanf:"previous_keys"`
}

// PreviousSigningKey carries one rotated-out key during the validation
// grace window. After grace expiry, remove from config.
type PreviousSigningKey struct {
	KeyID      string `koanf:"key_id"`
	SigningKey string `koanf:"signing_key" secret:"strong"`
}

// MaxRefreshGraceWindow caps Refresh.GraceWindow: the window in which a client
// that lost the response to a rotation may present the same token again. A
// replay is bound to the client that rotated (ADR 0068 D10) but a longer window is
// still a longer exposure for that client's own token; 2 minutes covers a slow
// mobile retry. 0 (strict rotation) stays valid.
const MaxRefreshGraceWindow = 120 * time.Second

// RefreshConfig — refresh-token lifetimes.
//
//   - AbsoluteTTL (default 14d, LEADKART_REFRESH__ABSOLUTE_TTL): a session ends this
//     long after the login that opened its family, however often it was rotated
//     (enforced in Family.RotateWithGrace from the family's created_at; a refresh
//     past it answers 401 refresh_rejected with reason session_expired and the
//     family is revoked with reason absolute_ttl). It is also the lifetime of each
//     refresh token, clamped to that deadline.
//   - SlidingTTL (default 24h, LEADKART_REFRESH__SLIDING_TTL): the IDLE timeout. Each
//     refresh token lives SlidingTTL from its issue (min(issued + SlidingTTL, family
//     created + AbsoluteTTL)); a rotation renews it, a grace replay does not. A
//     session not refreshed for that long answers 401 refresh_rejected with reason
//     session_expired and the family is revoked with reason idle_timeout. Validated:
//     positive, shorter than AbsoluteTTL, longer than GraceWindow. The 24h default is
//     the owner's ruling of 2026-11-21 ("a session unused for 24 hours ends"; it was 2h).
//   - GraceWindow (default 30s, LEADKART_REFRESH__GRACE_WINDOW, 0 = strict
//     rotation, at most [MaxRefreshGraceWindow]): ADR 0068 D10.
type RefreshConfig struct {
	AbsoluteTTL time.Duration `koanf:"absolute_ttl"`
	SlidingTTL  time.Duration `koanf:"sliding_ttl"`
	GraceWindow time.Duration `koanf:"grace_window"`
	FamilyCap   int           `koanf:"family_cap"`
}

// OTelConfig — OpenTelemetry exporter + service identity. Honors
// [OTEL_EXPORTER_OTLP_ENDPOINT] when present.
type OTelConfig struct {
	ServiceName    string  `koanf:"service_name"`
	ServiceVersion string  `koanf:"service_version"`
	OTLPEndpoint   string  `koanf:"otlp_endpoint"`
	SampleRatio    float64 `koanf:"sample_ratio"` // 0.0 to 1.0
}

// HTTPConfig — public listener behaviour.
//
// TrustedProxies is a comma-separated list of CIDRs / bare IPs of the reverse
// proxies (the web BFF, an ingress) whose X-Forwarded-For is believed (ADR
// 0030 / 0068 D11). Only a request whose TCP peer is in the list has its
// X-Forwarded-For read, rightmost-untrusted hop first. Empty (the default)
// trusts nobody: every client is its socket address, so behind a BFF all
// clients then share one rate-limit bucket. A /0 range is refused.
type HTTPConfig struct {
	TrustedProxies string `koanf:"trusted_proxies"`
}

// RouteLimit is one per-route, per-client token bucket: PerMinute is the
// steady refill rate, Burst the bucket size. Both must be positive.
type RouteLimit struct {
	PerMinute float64 `koanf:"per_minute"`
	Burst     int     `koanf:"burst"`
}

// RateLimitConfig — tighter per-client limits on the anonymous credential
// routes, on top of the global per-IP limiter (ADR 0068 D11). In-memory per
// process: with N replicas the effective limit is N times the setting.
type RateLimitConfig struct {
	Login                RouteLimit `koanf:"login"`                  // POST /api/v1/auth/login
	Refresh              RouteLimit `koanf:"refresh"`                // POST /api/v1/auth/refresh
	PasswordResetRequest RouteLimit `koanf:"password_reset_request"` // POST /api/v1/auth/request-password-reset
	PasswordReset        RouteLimit `koanf:"password_reset"`         // POST /api/v1/auth/reset-password
	EmailChangeConfirm   RouteLimit `koanf:"email_change_confirm"`   // POST /api/v1/auth/confirm-email-change
	TenantRegistration   RouteLimit `koanf:"tenant_registration"`    // POST /api/v1/tenants

	// ChangePassword is the per-PERSON budget of current-password checks on
	// POST /api/v1/auth/change-password (a signed-in route, so keyed by the
	// person, not the client address).
	ChangePassword RouteLimit `koanf:"change_password"`
}

// Argon2Config bounds concurrent password hashing and verification in this
// process (ADR 0068 D12). Each Argon2id operation holds ~19 MiB for its whole
// duration, so the API container's 1 GiB limit is reached by roughly fifty
// simultaneous logins; MaxConcurrent slots keep the worst case at
// MaxConcurrent x ~19.5 MiB. A request that cannot get a slot within
// QueueTimeout answers 503 + Retry-After.
//
// Two more bounds keep one source from starving everyone (fix round 12, item 3):
// MaxPerClient caps the slots one client key (the rate-limit key: the address, IPv6
// per /64) holds at once; its further operations wait their turn in a line of
// argon2.DefaultMaxWaiting places within QueueTimeout, and a full line is refused
// with the same 503; ReservedAuthenticated keeps that many slots for signed-in work
// (change-password, create user, a reset confirmation with a valid token), so
// anonymous login and registration can use at most MaxConcurrent - ReservedAuthenticated.
type Argon2Config struct {
	MaxConcurrent         int           `koanf:"max_concurrent"`
	QueueTimeout          time.Duration `koanf:"queue_timeout"`
	MaxPerClient          int           `koanf:"max_per_client"`
	ReservedAuthenticated int           `koanf:"reserved_authenticated"`
}

// Defaults returns the baseline AppConfig with safe non-production
// values. Loader merges this first, then file, then env; env wins.
func Defaults() AppConfig {
	return AppConfig{
		Env: "dev",
		Listen: ListenConfig{
			API:         ":8080",
			Admin:       ":9090",
			WorkerAdmin: ":9091",
		},
		Refresh: RefreshConfig{
			AbsoluteTTL: 14 * 24 * time.Hour,
			SlidingTTL:  24 * time.Hour,
			GraceWindow: 30 * time.Second,
			FamilyCap:   5,
		},
		OTel: OTelConfig{
			ServiceName:    "leadkart-api",
			ServiceVersion: "dev",
			SampleRatio:    1.0,
		},
		RateLimit: RateLimitConfig{
			Login:                RouteLimit{PerMinute: 30, Burst: 10},
			Refresh:              RouteLimit{PerMinute: 120, Burst: 30},
			PasswordResetRequest: RouteLimit{PerMinute: 5, Burst: 5},
			PasswordReset:        RouteLimit{PerMinute: 10, Burst: 5},
			EmailChangeConfirm:   RouteLimit{PerMinute: 10, Burst: 5},
			TenantRegistration:   RouteLimit{PerMinute: 10, Burst: 5},
			ChangePassword:       RouteLimit{PerMinute: 5, Burst: 5},
		},
		// 16 slots x ~19.5 MiB = ~312 MiB of live Argon2 memory (the Go collector needs
		// headroom on top of it, which is why production sets GOMEMLIMIT, see
		// docker/compose.prod.yml); at ~30-50 ms an operation that is ~300-500
		// verifications a second per core budget. 2 slots per client key (8 more may wait); 4 of the
		// 16 slots are kept for signed-in work.
		Argon2: Argon2Config{MaxConcurrent: 16, QueueTimeout: 2 * time.Second, MaxPerClient: 2, ReservedAuthenticated: 4},
	}
}

// Load builds an [AppConfig] from defaults + optional YAML file + env.
//
// Returns the parsed config + a non-nil error if any provider failed
// to load OR if [Validate] rejects the merged result.
func Load() (AppConfig, error) { return load(Validate) }

// LoadWorker is [Load] for the worker binary: the same sources, validated with
// [ValidateWorker] (it has no HTTP listener, no rate limits, no password work and
// no phantom lockout key, so a production worker without a trusted-proxy decision
// boots).
func LoadWorker() (AppConfig, error) { return load(ValidateWorker) }

func load(validate func(AppConfig) error) (AppConfig, error) {
	k := koanf.New(".")

	cfg := Defaults()
	if err := k.Load(structProvider(cfg), nil); err != nil {
		return AppConfig{}, fmt.Errorf("config: load defaults: %w", err)
	}

	if path := os.Getenv(configFileEnvVar); path != "" {
		if err := k.Load(fileprovider.Provider(path), yaml.Parser()); err != nil {
			return AppConfig{}, fmt.Errorf("config: load %q: %w", path, err)
		}
	}

	envOpts := envprovider.Opt{
		Prefix: EnvPrefix,
		// `LEADKART_POSTGRES__DSN` → `postgres.dsn`. The first arg
		// to Provider below is the delimiter koanf uses to UN-flatten
		// the lowercase dotted keys into the nested struct tree.
		TransformFunc: func(key, value string) (string, any) {
			key = strings.TrimPrefix(key, EnvPrefix)
			key = strings.ReplaceAll(key, EnvSeparator, ".")
			return strings.ToLower(key), value
		},
	}
	if err := k.Load(envprovider.Provider(".", envOpts), nil); err != nil {
		return AppConfig{}, fmt.Errorf("config: load env: %w", err)
	}

	var out AppConfig
	if err := k.UnmarshalWithConf("", &out, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return AppConfig{}, fmt.Errorf("config: unmarshal: %w", err)
	}

	if err := validate(out); err != nil {
		return AppConfig{}, err
	}
	return out, nil
}

// structProvider adapts an [AppConfig] value into a koanf provider so
// Defaults() can layer underneath file + env without a YAML file.
//
// Implements koanf.Provider with a manual YAML round-trip — koanf's
// own struct provider is in a separate package; this avoids the dep.
func structProvider(cfg AppConfig) koanf.Provider {
	return &inMemoryProvider{cfg: cfg}
}

type inMemoryProvider struct {
	cfg AppConfig
}

func (p *inMemoryProvider) ReadBytes() ([]byte, error) {
	return yaml.Parser().Marshal(structToMap(p.cfg))
}

func (p *inMemoryProvider) Read() (map[string]any, error) {
	return structToMap(p.cfg), nil
}

// structToMap walks AppConfig fields → map[string]any, honouring
// `koanf` struct tags. Hand-rolled because reflection would pull in
// koanf-internal helpers we don't depend on directly.
func structToMap(cfg AppConfig) map[string]any {
	previous := make([]map[string]any, len(cfg.JWT.PreviousKeys))
	for i, p := range cfg.JWT.PreviousKeys {
		previous[i] = map[string]any{"key_id": p.KeyID, "signing_key": p.SigningKey}
	}
	rl := func(l RouteLimit) map[string]any {
		return map[string]any{"per_minute": l.PerMinute, "burst": l.Burst}
	}
	return map[string]any{
		"env":  cfg.Env,
		"http": map[string]any{"trusted_proxies": cfg.HTTP.TrustedProxies},
		"rate_limit": map[string]any{
			"login":                  rl(cfg.RateLimit.Login),
			"refresh":                rl(cfg.RateLimit.Refresh),
			"password_reset_request": rl(cfg.RateLimit.PasswordResetRequest),
			"password_reset":         rl(cfg.RateLimit.PasswordReset),
			"email_change_confirm":   rl(cfg.RateLimit.EmailChangeConfirm),
			"tenant_registration":    rl(cfg.RateLimit.TenantRegistration),
			"change_password":        rl(cfg.RateLimit.ChangePassword),
		},
		"argon2": map[string]any{
			"max_concurrent": cfg.Argon2.MaxConcurrent,
			"queue_timeout":  cfg.Argon2.QueueTimeout.String(),
			"max_per_client": cfg.Argon2.MaxPerClient, "reserved_authenticated": cfg.Argon2.ReservedAuthenticated,
		},
		"identity": map[string]any{"lockout_key": cfg.Identity.LockoutKey},
		"listen": map[string]any{
			"api":          cfg.Listen.API,
			"admin":        cfg.Listen.Admin,
			"worker_admin": cfg.Listen.WorkerAdmin,
		},
		"postgres": map[string]any{"dsn": cfg.Postgres.DSN},
		"redis": map[string]any{
			"addr": cfg.Redis.Addr, "password": cfg.Redis.Password, "db": cfg.Redis.DB,
		},
		"jwt": map[string]any{
			"key_id": cfg.JWT.KeyID, "signing_key": cfg.JWT.SigningKey,
			"previous_keys": previous,
		},
		"refresh": map[string]any{
			"absolute_ttl": cfg.Refresh.AbsoluteTTL.String(),
			"sliding_ttl":  cfg.Refresh.SlidingTTL.String(),
			"grace_window": cfg.Refresh.GraceWindow.String(),
			"family_cap":   cfg.Refresh.FamilyCap,
		},
		"otel": map[string]any{
			"service_name":    cfg.OTel.ServiceName,
			"service_version": cfg.OTel.ServiceVersion,
			"otlp_endpoint":   cfg.OTel.OTLPEndpoint,
			"sample_ratio":    cfg.OTel.SampleRatio,
		},
	}
}

// Common errors surface as exported sentinels so the `cmd/api` startup
// can branch on them (e.g. log a friendlier "you forgot to set
// $LEADKART_POSTGRES__DSN").

// ErrMissingPostgres is returned when Postgres.DSN is empty.
var ErrMissingPostgres = errors.New("config: Postgres.DSN required (set LEADKART_POSTGRES__DSN)")

// ErrMissingRedis is returned when Redis.Addr is empty.
var ErrMissingRedis = errors.New("config: Redis.Addr required (set LEADKART_REDIS__ADDR)")

// ErrMissingJWTKeyID is returned when JWT.KeyID is empty.
var ErrMissingJWTKeyID = errors.New("config: JWT.KeyID required (set LEADKART_JWT__KEY_ID)")

// ErrJWTKeyTooShort is returned when JWT.SigningKey < 32 bytes (RFC 7518 §3.2).
var ErrJWTKeyTooShort = errors.New("config: JWT.SigningKey must be ≥32 bytes (RFC 7518 §3.2)")

// ErrMissingLockoutKey is returned in production when Identity.LockoutKey is empty.
var ErrMissingLockoutKey = errors.New("config: Identity.LockoutKey required in production (set LEADKART_IDENTITY__LOCKOUT_KEY to `openssl rand -base64 32`)")

// ErrPlaceholderSecret is returned when a `secret:` field carries a
// known-placeholder value (e.g. `CHANGE_ME`, dev defaults). Refuses
// boot per [security.md] "Secrets integrity — startup validator".
var ErrPlaceholderSecret = errors.New("config: placeholder secret detected — refusing to boot")
