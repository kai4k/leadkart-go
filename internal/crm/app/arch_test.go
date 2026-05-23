// arch_test.go — CRM-module boundary discipline per ADR 0047. Mirror
// of internal/identity/app/arch_test.go.
//
// The CRM app/ layer is a pure-Go region that depends on domain
// repository interfaces + cross-cutting infra interfaces (pg.UnitOfWork,
// pagination) — never on the persistence substrate. This test walks
// every non-test .go file under internal/crm/app/ and fails on any
// forbidden import.
//
// Per CLAUDE.md "drift = finding": if this test fails, fix the import
// (route through an interface) — DO NOT amend the allowlist without an
// ADR amendment.

package app_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

var forbiddenAppImports = map[string]string{
	"github.com/leadkart/leadkart-go/internal/crm/adapters/db": "sqlc-generated row types leak DB shape into the application layer; use a domain-side Reader/Repository interface instead",
	"github.com/leadkart/leadkart-go/internal/crm/adapters":    "concrete adapter package leaks DB driver + sqlc; handlers must accept domain repository interfaces per Cheney",
	"github.com/jackc/pgx/v5":                                  "pgx driver is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgxpool":                          "pgxpool is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgtype":                           "pgtype is a sqlc/driver concern; should never appear in app/ — domain VOs carry strong types",
}

// TestArch_AppDoesNotImportForbidden walks every Go file under
// internal/crm/app/ (excluding _test.go) and asserts none of them
// import a path on the forbidden list.
func TestArch_AppDoesNotImportForbidden(t *testing.T) {
	t.Parallel()

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
				violations = append(violations, violation{file: path, imp: imPath, reason: reason})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(violations) > 0 {
		t.Logf("BOUNDARY VIOLATIONS — %d forbidden imports in internal/crm/app/", len(violations))
		t.Logf("Per ADR 0047: app/ must depend on domain + platform interfaces.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s\n  why blocked: %s", v.file, v.imp, v.reason)
		}
	}
}
