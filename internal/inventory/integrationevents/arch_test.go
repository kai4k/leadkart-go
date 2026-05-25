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
// "Event versioning". Module prefix MUST be `inventory`; event-name
// segment lower-snake-case; trailing `.vN` integer.
var aliasRegex = regexp.MustCompile(`^inventory\.[a-z][a-z0-9_]*\.v\d+$`)

// TestArch_AllRegisteredEventsSatisfyMarker enforces that every event
// in the registry implements TenantScoped. Inventory has no Platform-
// scoped events at v0.2 — every aggregate is tenant-scoped per BRD §6.5.
func TestArch_AllRegisteredEventsSatisfyMarker(t *testing.T) {
	t.Parallel()
	if len(all()) == 0 {
		t.Fatal("no events registered — registry init order broken?")
	}
	for _, e := range all() {
		if _, isTenant := e.(TenantScoped); !isTenant {
			t.Errorf("%T: must implement TenantScoped", e)
		}
	}
}

// TestArch_AllRegisteredEventsHaveCanonicalAlias asserts every Topic()
// matches the `inventory.{event-kebab}.v{N}` regex.
func TestArch_AllRegisteredEventsHaveCanonicalAlias(t *testing.T) {
	t.Parallel()
	seen := map[string]string{} // alias -> typeName
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

// TestArch_EveryRecordEndingInVNRegistered enforces that every exported
// type whose name matches *V{N} (e.g. ProductCreatedV1) is registered
// in the catalogue.
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
		src, err := os.ReadFile(f) //nolint:gosec // arch-test fixture path
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
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

// TestArch_NoFrameworkImports enforces the "Unobtrusive Mode" canon per
// messaging.md: framework-neutral records, zero infrastructure deps.
// Allowed imports: stdlib + uuid + sibling domain packages (mapping.go
// reads domain events to translate them) + identity/domain/tenant (for
// the cross-aggregate TenantID reference VOs use as a type tag).
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
		"github.com/leadkart/leadkart-go/internal/inventory/adapters",
		"github.com/leadkart/leadkart-go/internal/inventory/app",
		"github.com/leadkart/leadkart-go/internal/inventory/ports",
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

// TestArch_TenantScopedRecordsExposeTenantID verifies every TenantScoped
// record's method exists + returns a uuid.UUID (compile-time covered).
func TestArch_TenantScopedRecordsExposeTenantID(t *testing.T) {
	t.Parallel()
	seen := 0
	for _, e := range all() {
		ts, ok := e.(TenantScoped)
		if !ok {
			continue
		}
		_ = ts.TenantID() // arch-test:ignore-err — method-existence proof; value discarded by design
		seen++
	}
	require.Positive(t, seen, "no TenantScoped records discovered; the TenantScoped contract has zero implementers")
}

// packageDir returns the absolute path of this test's package directory.
func packageDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(here)
}
