package integrationevents

import (
	"go/build"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// aliasRegex enforces the canonical wire-alias shape per messaging.md
// "Event versioning". Module prefix MUST be `tasks`; event-name
// segment lower-case + dashes / underscores; trailing `.vN` integer.
var aliasRegex = regexp.MustCompile(`^tasks\.[a-z][a-z0-9_-]*\.v\d+$`)

func TestArch_AllRegisteredEventsSatisfyMarker(t *testing.T) {
	t.Parallel()
	if len(all()) == 0 {
		t.Fatal("no events registered — registry init order broken?")
	}
	for _, e := range all() {
		if _, ok := e.(TenantScoped); !ok {
			t.Errorf("%T: must implement TenantScoped", e)
		}
	}
}

func TestArch_AllRegisteredEventsHaveCanonicalAlias(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, e := range all() {
		alias := e.Topic()
		typeName := reflect.TypeOf(e).Name()
		if !aliasRegex.MatchString(alias) {
			t.Errorf("%s: alias %q does not match %s", typeName, alias, aliasRegex.String())
		}
		if existing, dup := seen[alias]; dup {
			t.Errorf("alias collision: %q used by both %s and %s", alias, existing, typeName)
		}
		seen[alias] = typeName
	}
}

func TestArch_EveryRecordEndingInVNRegistered(t *testing.T) {
	t.Parallel()
	registered := map[string]struct{}{}
	for _, e := range all() {
		registered[reflect.TypeOf(e).Name()] = struct{}{}
	}

	pkgDir := packageDir(t)
	files, err := filepath.Glob(filepath.Join(pkgDir, "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	versionTyped := regexp.MustCompile(`^type\s+([A-Z]\w*V\d+)\s+struct\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f) //nolint:gosec // arch-test fixture path
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			line = strings.TrimSpace(line)
			m := versionTyped.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			typeName := m[1]
			if _, ok := registered[typeName]; !ok {
				t.Errorf("%s defined but not registered — add `_ = register(%s{})` in %s",
					typeName, typeName, filepath.Base(f))
			}
		}
	}
}

func TestArch_NoFrameworkImports(t *testing.T) {
	t.Parallel()
	pkgDir := packageDir(t)
	pkg, err := build.ImportDir(pkgDir, build.ImportComment)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}
	banned := []string{
		"github.com/ThreeDotsLabs/watermill",
		"github.com/jackc/pgx",
		"github.com/redis/go-redis",
		"github.com/dgraph-io/ristretto",
		"github.com/golang-jwt/jwt",
		"net/http",
		"github.com/leadkart/leadkart-go/internal/tasks/adapters",
		"github.com/leadkart/leadkart-go/internal/tasks/app",
		"github.com/leadkart/leadkart-go/internal/tasks/ports",
	}
	for _, imp := range pkg.Imports {
		for _, ban := range banned {
			if strings.HasPrefix(imp, ban) {
				t.Errorf("forbidden import %q (matches ban %q) — integrationevents must stay framework-neutral",
					imp, ban)
			}
		}
	}
}

func TestArch_TenantScopedRecordsExposeTenantID(t *testing.T) {
	t.Parallel()
	var seen int
	for _, e := range all() {
		ts, ok := e.(TenantScoped)
		if !ok {
			continue
		}
		_ = ts.TenantID() // arch-test:ignore-err — TenantID() returns uuid.UUID (no error).
		seen++
	}
	require.Positive(t, seen, "no TenantScoped records discovered — registry empty or interface mis-named")
}

func packageDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(here)
}
