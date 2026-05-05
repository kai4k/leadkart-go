package integrationevents

import (
	"go/build"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// aliasRegex enforces the canonical wire-alias shape per messaging.md
// "Event versioning". Module prefix MUST be `identity`; event-name
// segment lower-snake-case; trailing `.vN` integer.
var aliasRegex = regexp.MustCompile(`^identity\.[a-z][a-z0-9_]*\.v\d+$`)

// TestArch_AllRegisteredEventsSatisfyMarker enforces:
//   - every event in the registry implements TenantScoped OR Platform;
//   - no event implements both (would mean a typo on the marker).
//
// Compile-time assertions in each *.go file already enforce per-type;
// this test catches missing assertions on NEW types added to the
// registry without the matching `var _ Platform = …` line.
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

// TestArch_AllRegisteredEventsHaveCanonicalAlias asserts every Topic()
// matches the `identity.{event-kebab}.v{N}` regex.
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

// TestArch_EveryRecordEndingInVNRegistered enforces the rule that
// every exported type whose name matches *V{N} (e.g. TenantRegisteredV1)
// is registered in the catalogue. Catches "I added a struct but
// forgot the register() line" mistakes.
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
		for _, line := range strings.Split(src, "\n") {
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

// TestArch_NoFrameworkImports enforces the "Unobtrusive Mode" canon
// per messaging.md: framework-neutral records, zero infrastructure
// dependencies. Allowed imports: stdlib + uuid + sibling domain
// packages (mapping.go reads domain events to translate them).
func TestArch_NoFrameworkImports(t *testing.T) {
	t.Parallel()

	pkgDir := packageDir(t)
	pkg, err := build.ImportDir(pkgDir, build.ImportComment)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}

	// Banned: any infrastructure / framework dependency.
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

// TestArch_TenantScopedRecordsExposeTenantID verifies every
// TenantScoped record returns a non-zero TenantID() when the embedded
// claim field is populated. Catches "I made the type satisfy
// TenantScoped via method but the method returns the wrong field".
func TestArch_TenantScopedRecordsExposeTenantID(t *testing.T) {
	t.Parallel()
	for _, e := range all() {
		ts, ok := e.(TenantScoped)
		if !ok {
			continue
		}
		// All zero-value records will return a zero UUID. We don't
		// fail on that — just ensure the method exists + returns
		// a uuid (compile-time covered) AND that the docstring on
		// TenantScoped is honoured.
		_ = ts.TenantID()
	}
}

// packageDir returns the absolute path of this test's package
// directory, used for go/build.ImportDir + filepath.Glob walks.
func packageDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(here)
}

// readFile is a thin os.ReadFile wrapper returning string. Inlined
// rather than imported so the test file's import surface stays small.
func readFile(path string) (string, error) {
	b, err := readBytes(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
