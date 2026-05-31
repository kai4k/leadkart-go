package integrationevents

import (
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"

	"github.com/stretchr/testify/require"
)

// aliasRegex enforces the wire-alias shape (messaging.md "Event versioning"):
// `platform` prefix, lower-snake-case name, trailing `.vN`.
var aliasRegex = regexp.MustCompile(`^platform\.[a-z][a-z0-9_]*\.v\d+$`)

// TestArch_AllRegisteredEventsSatisfyMarker asserts every registered event
// implements exactly one of TenantScoped or Platform.
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
// aliasRegex and aliases are collision-free.
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

// TestArch_EveryRecordEndingInVNRegistered asserts every exported
// `type FooV{N} struct` appears in the registry — catches a forgotten register().
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
		src, err := readFileString(f)
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

// TestArch_NoFrameworkImports enforces framework-neutrality: records may import
// only stdlib + uuid + sibling Platform domain packages. No watermill, pgx,
// http, or app/adapter imports.
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
		"github.com/leadkart/leadkart-go/internal/platform/adapters",
		"github.com/leadkart/leadkart-go/internal/platform/app",
		"github.com/leadkart/leadkart-go/internal/platform/ports",
		// Cross-context ban: Platform events must not lean on Identity internals.
		"github.com/leadkart/leadkart-go/internal/identity/adapters",
		"github.com/leadkart/leadkart-go/internal/identity/app",
		"github.com/leadkart/leadkart-go/internal/identity/ports",
	}
	for _, imp := range pkg.Imports {
		for _, ban := range banned {
			if strings.HasPrefix(imp, ban) {
				t.Errorf("forbidden import %q (matches ban %q) — integration events must stay framework-neutral",
					imp, ban)
			}
		}
	}
}

// TestArch_TenantScopedRecordsExposeTenantID smoke-checks that TenantScoped
// records honour the interface contract. UUIDs travel as strings per ADR 0059.
func TestArch_TenantScopedRecordsExposeTenantID(t *testing.T) {
	t.Parallel()
	seen := 0
	for _, e := range all() {
		ts, ok := e.(TenantScoped)
		if !ok {
			continue
		}
		_ = ts.TenantIDString() // arch-test:ignore-err — smoke check; interface satisfaction is the load-bearing surface
		seen++
	}
	require.Positive(t, seen, "no TenantScoped records discovered; the TenantScoped contract has zero implementers")
}

// TestArch_FromDomainEvent_HandlesAllRegisteredDomainEvents asserts
// FromDomainEvent recognises every Platform domain-event type: each must return
// a non-nil Event or (nil, nil); (nil, ErrUnknown) fails CI. (Slice 1 H6 fix:
// new events can no longer silently double or vanish.)
//
// Maintenance: when a domain event type is added under platform/domain/*, append
// a zero-value sample below and add its case to FromDomainEvent.
func TestArch_FromDomainEvent_HandlesAllRegisteredDomainEvents(t *testing.T) {
	t.Parallel()

	// One zero-value sample per recognised domain event type. Only the type
	// switch fires; field values are not inspected.
	samples := []any{
		unverifiedcontact.CreatedEvent{},
		unverifiedcontact.CallStartedEvent{},
		unverifiedcontact.VerifiedEvent{},
		unverifiedcontact.RejectedEvent{},
		unverifiedcontact.MarkedBusyEvent{},
		verificationcall.LoggedEvent{},
		platformlead.VerifiedEvent{},
		platformlead.PurchasedEvent{},
		leadcredit.AdjustedEvent{},
	}

	for _, s := range samples {
		s := s // capture for parallel subtest
		name := reflect.TypeOf(s).String()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ie, err := FromDomainEvent(s)
			if errors.Is(err, ErrUnknown) {
				t.Fatalf("%s: FromDomainEvent returned ErrUnknown — add a case (or documented suppression) in mapping.go", name)
			}
			if err != nil {
				t.Fatalf("%s: FromDomainEvent unexpected error: %v", name, err)
			}
			// ie may be nil — intentional suppression per mapping.go.
			_ = ie
		})
	}
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

// readFileString reads a file into a string.
func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // arch-test fixture path
	if err != nil {
		return "", err
	}
	return string(b), nil
}
