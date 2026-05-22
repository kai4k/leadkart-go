// arch_test.go — architectural boundary discipline as a CI gate.
//
// Per ADR 0047: the app/ layer is a pure-Go region that depends on
// domain interfaces + a handful of platform-cross-cutting interfaces
// (pg.UnitOfWork, audit.Reader, etc.) but MUST NOT depend on:
//
//   - github.com/leadkart/leadkart-go/internal/identity/adapters/db
//     (sqlc-generated row types — DB-shape leaks the persistence model
//     into the application layer)
//   - github.com/jackc/pgx/v5 or pgx/v5/pgxpool or pgx/v5/pgtype
//     (DB driver — leaks the substrate)
//   - github.com/leadkart/leadkart-go/internal/identity/adapters
//     (concrete adapter package — handlers must accept INTERFACES
//     per Cheney; concrete repository structs are an inversion)
//
// This test walks every .go file under internal/identity/app/ (test
// files included — fixtures may legitimately import adapters for
// wiring fakes, BUT non-test files MUST NOT). On violation, the test
// names the file + the forbidden import + the rule.
//
// Per CLAUDE.md "drift = finding": if this test fails, fix the import
// (route through an interface) — DO NOT add the import to the
// allowlist without an accompanying ADR amendment.

package app_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenAppImports lists import paths that the app/ layer MUST NOT
// depend on. The map key is the import path; the value is the reason
// the rule exists (surfaced in failure messages so the violator
// understands why).
var forbiddenAppImports = map[string]string{
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db": "sqlc-generated row types leak DB shape into the application layer; use a domain-side Reader/Repository interface instead",
	"github.com/leadkart/leadkart-go/internal/identity/adapters":    "concrete adapter package leaks DB driver + sqlc; handlers must accept domain repository interfaces per Cheney",
	"github.com/jackc/pgx/v5":                                       "pgx driver is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgxpool":                               "pgxpool is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgtype":                                "pgtype is a sqlc/driver concern; should never appear in app/ — domain VOs carry strong types",
}

// TestArch_AppDoesNotImportForbidden walks every Go file under
// internal/identity/app/ (excluding _test.go files — test fixtures are
// permitted to import adapters for wiring) and asserts none of them
// import a path on the forbidden list.
func TestArch_AppDoesNotImportForbidden(t *testing.T) {
	t.Parallel()

	// Scoped to internal/identity/app/ (this file's directory tree).
	// The arch_test.go sits in the app/ package root, so "." walks
	// app/ + all subpackages (command/, query/, etc.).
	root := filepath.Clean(".")
	fset := token.NewFileSet()

	type violation struct {
		file   string
		imp    string
		reason string
	}
	var violations []violation

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Test files are exempt — fixtures may import adapters to wire
		// real repository implementations against testcontainers. The
		// production code path is what the rule guards.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, im := range f.Imports {
			imPath := strings.Trim(im.Path.Value, `"`)
			if reason, bad := forbiddenAppImports[imPath]; bad {
				violations = append(violations, violation{
					file:   path,
					imp:    imPath,
					reason: reason,
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(violations) > 0 {
		t.Logf("BOUNDARY VIOLATIONS — %d forbidden imports in internal/identity/app/", len(violations))
		t.Logf("Per ADR 0047: app/ must depend on domain + platform interfaces, never on")
		t.Logf("the persistence substrate or its sqlc-generated row types.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s\n  why blocked: %s", v.file, v.imp, v.reason)
		}
	}
}
