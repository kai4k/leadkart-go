package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/config"
)

// The local compose stack boots the api as the application role with the DSN that
// docker/compose.yml defaults to. That DSN must pass the same start-up validation as
// any other (a password that looks like "placeholder" is refused), and it must be the
// password docker/postgres/init.sql gives the role, or the dev stack cannot start
// (found when the local runbook was run: the api refused to boot).
func TestDevStack_DefaultApplicationDSNPassesValidationAndMatchesTheRole(t *testing.T) {
	t.Parallel()
	read := func(parts ...string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(append([]string{"..", "..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	compose := regexp.MustCompile(`postgres://leadkart_app:([^@]+)@postgres:5432/leadkart\?sslmode=disable`).FindStringSubmatch(read("docker", "compose.yml"))
	if compose == nil {
		t.Fatal("docker/compose.yml: no default application-role DSN found")
	}
	initSQL := regexp.MustCompile(`(?s)CREATE ROLE leadkart_app.*?PASSWORD '([^']+)'`).FindStringSubmatch(read("docker", "postgres", "init.sql"))
	if initSQL == nil {
		t.Fatal("docker/postgres/init.sql: leadkart_app password not found")
	}
	if compose[1] != initSQL[1] {
		t.Fatalf("compose DSN password %q differs from the role's password %q in init.sql", compose[1], initSQL[1])
	}
	cfg := validBaseConfig()
	cfg.Postgres.DSN = compose[0]
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("the default dev DSN fails start-up validation: %v", err)
	}
}
