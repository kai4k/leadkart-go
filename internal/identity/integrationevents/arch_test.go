package integrationevents

import (
	"go/build"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// aliasRegex enforces the canonical wire-alias shape (messaging.md
// "Event versioning"): identity prefix, lower-snake-case event name, .vN suffix.
var aliasRegex = regexp.MustCompile(`^identity\.[a-z][a-z0-9_]*\.v\d+$`)

// TestArch_AllRegisteredEventsSatisfyMarker enforces that every registered
// event implements TenantScoped OR Platform (not both). Compile-time
// assertions per file cover existing types; this catches newly added types
// missing the marker assertion.
func TestArch_AllRegisteredEventsSatisfyMarker(t *testing.T) {
	t.Parallel()
	if len(all()) == 0 {
		t.Fatal("no events registered — registry init order broken?")
	}
	for _, e := range all() {
		_, isTenant := e.(TenantScoped)
		_, isPlatform := e.(Platform)
		if !isTenant && !isPlatform {
			t.Errorf("%T: must implement TenantScoped OR Platform", e)
		}
		if isTenant && isPlatform {
			t.Errorf("%T: must implement EXACTLY ONE of TenantScoped or Platform (got both)", e)
		}
	}
}

// TestArch_AllRegisteredEventsHaveCanonicalAlias asserts every Topic() matches
// the canonical alias regex and that no two events share an alias.
func TestArch_AllRegisteredEventsHaveCanonicalAlias(t *testing.T) {
	t.Parallel()
	seen := map[string]string{} // alias -> typeName, for collision detection
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

// TestArch_EveryRecordEndingInVNRegistered enforces that every exported VN
// struct is in the catalogue. Catches "added a struct but forgot register()".
func TestArch_EveryRecordEndingInVNRegistered(t *testing.T) {
	t.Parallel()

	registered := map[string]struct{}{}
	for _, e := range all() {
		registered[reflect.TypeOf(e).Name()] = struct{}{}
	}

	// Walk all *.go files in this package looking for `type FooV1 struct`.
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
		src, err := readFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for line := range strings.SplitSeq(src, "\n") {
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

// TestArch_NoFrameworkImports enforces the Unobtrusive Mode canon: no
// infrastructure imports. Allowed: stdlib + uuid + sibling domain packages.
func TestArch_NoFrameworkImports(t *testing.T) {
	t.Parallel()

	pkgDir := packageDir(t)
	pkg, err := build.ImportDir(pkgDir, build.ImportComment)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}

	// Banned infrastructure/framework imports.
	banned := []string{
		"github.com/ThreeDotsLabs/watermill",
		"github.com/jackc/pgx",
		"github.com/redis/go-redis",
		"github.com/dgraph-io/ristretto",
		"github.com/golang-jwt/jwt",
		"net/http",
		"github.com/leadkart/leadkart-go/internal/platform",
		"github.com/leadkart/leadkart-go/internal/identity/adapters",
		"github.com/leadkart/leadkart-go/internal/identity/app",
		"github.com/leadkart/leadkart-go/internal/identity/ports",
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
// record exposes TenantID() callable on a zero value without panic.
func TestArch_TenantScopedRecordsExposeTenantID(t *testing.T) {
	t.Parallel()
	seen := 0
	for _, e := range all() {
		ts, ok := e.(TenantScoped)
		if !ok {
			continue
		}
		// Zero UUID is fine — gates the contract surface, not the value.
		_ = ts.TenantID() // arch-test:ignore-err — contract surface check; value discarded by design
		seen++
	}
	require.Positive(t, seen, "no TenantScoped records discovered; the TenantScoped contract has zero implementers, which contradicts the integration-events catalog")
}

// packageDir returns the absolute path of this package for ImportDir/Glob walks.
func packageDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(here)
}

// readFile wraps readBytes as string for arch-test source scanning.
func readFile(path string) (string, error) {
	b, err := readBytes(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
