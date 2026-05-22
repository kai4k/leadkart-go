package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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
		"LEADKART_POSTGRES__DSN":   "postgres://leadkart_app:pw@localhost:5432/leadkart?sslmode=disable",
		"LEADKART_REDIS__ADDR":     "localhost:6379",
		"LEADKART_JWT__KEY_ID":     "k1",
		"LEADKART_JWT__SIGNING_KEY": validJWTKey,
	}
}

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

func TestLoad_FileThenEnv(t *testing.T) {
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "leadkart.yaml")
	yaml := `
env: production
listen:
  api: ":7000"
otel:
  service_name: leadkart-from-file
  sample_ratio: 0.25
`
	writeFile(t, yamlFile, yaml)

	setEnv(t, minRequired())
	t.Setenv("LEADKART_CONFIG_FILE", yamlFile)
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

func TestLoad_RejectsMissingPostgres(t *testing.T) {
	required := minRequired()
	delete(required, "LEADKART_POSTGRES__DSN")
	setEnv(t, required)
	_, err := config.Load()
	if !errors.Is(err, config.ErrMissingPostgres) {
		t.Fatalf("expected ErrMissingPostgres, got %v", err)
	}
}

func TestLoad_RejectsShortJWTKey(t *testing.T) {
	required := minRequired()
	required["LEADKART_JWT__SIGNING_KEY"] = "too-short"
	setEnv(t, required)
	_, err := config.Load()
	if !errors.Is(err, config.ErrJWTKeyTooShort) {
		t.Fatalf("expected ErrJWTKeyTooShort, got %v", err)
	}
}

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
