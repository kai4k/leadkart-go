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
	Env      string         `koanf:"env"`       // "dev" | "staging" | "production"
	Listen   ListenConfig   `koanf:"listen"`
	Postgres PostgresConfig `koanf:"postgres"`
	Redis    RedisConfig    `koanf:"redis"`
	JWT      JWTConfig      `koanf:"jwt"`
	Refresh  RefreshConfig  `koanf:"refresh"`
	OTel     OTelConfig     `koanf:"otel"`
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
	Addr     string `koanf:"addr"`                     // "localhost:6379"
	Password string `koanf:"password" secret:"weak"`   // optional
	DB       int    `koanf:"db"`                       // 0-15
}

// JWTConfig — HS256 signing key + kid header per [security.md] "JWT
// signing key + key history". Production rotates by populating
// `PreviousKeys` for the access-token-lifetime × 2 grace window.
type JWTConfig struct {
	KeyID        string                `koanf:"key_id"`
	SigningKey   string                `koanf:"signing_key" secret:"strong"`
	PreviousKeys []PreviousSigningKey  `koanf:"previous_keys"`
}

// PreviousSigningKey carries one rotated-out key during the validation
// grace window. After grace expiry, remove from config.
type PreviousSigningKey struct {
	KeyID      string `koanf:"key_id"`
	SigningKey string `koanf:"signing_key" secret:"strong"`
}

// RefreshConfig — refresh-token TTLs. Default 14d absolute / 2h sliding
// per [security.md] "Refresh token — family pattern".
type RefreshConfig struct {
	AbsoluteTTL time.Duration `koanf:"absolute_ttl"`
	SlidingTTL  time.Duration `koanf:"sliding_ttl"`
	GraceWindow time.Duration `koanf:"grace_window"`
	FamilyCap   int           `koanf:"family_cap"`
}

// OTelConfig — OpenTelemetry exporter + service identity. Honors
// [OTEL_EXPORTER_OTLP_ENDPOINT] when present.
type OTelConfig struct {
	ServiceName     string `koanf:"service_name"`
	ServiceVersion  string `koanf:"service_version"`
	OTLPEndpoint    string `koanf:"otlp_endpoint"`
	SampleRatio     float64 `koanf:"sample_ratio"` // 0.0 to 1.0
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
			SlidingTTL:  2 * time.Hour,
			GraceWindow: 30 * time.Second,
			FamilyCap:   5,
		},
		OTel: OTelConfig{
			ServiceName:    "leadkart-api",
			ServiceVersion: "dev",
			SampleRatio:    1.0,
		},
	}
}

// Load builds an [AppConfig] from defaults + optional YAML file + env.
//
// Returns the parsed config + a non-nil error if any provider failed
// to load OR if [Validate] rejects the merged result.
func Load() (AppConfig, error) {
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

	if err := Validate(out); err != nil {
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
	return map[string]any{
		"env": cfg.Env,
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

// ErrPlaceholderSecret is returned when a `secret:` field carries a
// known-placeholder value (e.g. `CHANGE_ME`, dev defaults). Refuses
// boot per [security.md] "Secrets integrity — startup validator".
var ErrPlaceholderSecret = errors.New("config: placeholder secret detected — refusing to boot")
