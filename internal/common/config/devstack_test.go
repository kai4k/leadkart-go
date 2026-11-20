package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
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

// Every rate-limit and Argon2 setting must reach the api container in BOTH compose
// files, with the code's default as the compose default: a knob that
// docker/.env.example documents but the compose file does not pass changes nothing
// (found when the local runbook relaxed the login limit and the setting was silently
// ignored). Names and defaults are derived from config.Defaults(), so adding a field
// without wiring it here fails this test.
func TestDevStack_ComposeFilesPassEveryRateLimitAndArgon2Setting(t *testing.T) {
	t.Parallel()
	d := config.Defaults()
	want := map[string]string{}
	rl := reflect.ValueOf(d.RateLimit)
	for i := range rl.NumField() {
		name := strings.ToUpper(rl.Type().Field(i).Tag.Get("koanf"))
		lim := rl.Field(i).Interface().(config.RouteLimit)
		want["LEADKART_RATE_LIMIT__"+name+"__PER_MINUTE"] = strconv.FormatFloat(lim.PerMinute, 'f', -1, 64)
		want["LEADKART_RATE_LIMIT__"+name+"__BURST"] = strconv.Itoa(lim.Burst)
	}
	want["LEADKART_ARGON2__MAX_CONCURRENT"] = strconv.Itoa(d.Argon2.MaxConcurrent)
	want["LEADKART_ARGON2__QUEUE_TIMEOUT"] = d.Argon2.QueueTimeout.String()
	want["LEADKART_ARGON2__MAX_PER_CLIENT"] = strconv.Itoa(d.Argon2.MaxPerClient)
	want["LEADKART_ARGON2__RESERVED_AUTHENTICATED"] = strconv.Itoa(d.Argon2.ReservedAuthenticated)

	for _, file := range []string{"compose.yml", "compose.prod.yml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "docker", file))
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for name, def := range want {
			line := name + `: "${` + name + `:-` + def + `}"`
			if !strings.Contains(text, line) {
				t.Errorf("docker/%s does not pass %s with the code default; want the line %s", file, name, line)
			}
		}
	}
}
