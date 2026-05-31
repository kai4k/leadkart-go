// Architectural boundary discipline as a CI gate.
//
// Per ADR 0047 + ADR 0059: app/ depends on domain + cross-cutting infra
// interfaces (pg.UnitOfWork, etc.), never on the persistence substrate:
// adapters/db (sqlc rows), the adapters package, or pgx/pgxpool/pgtype.
//
// Mirror of internal/identity/app/arch_test.go.

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
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db": "sqlc-generated row types leak DB shape into the application layer; use a domain-side Reader/Repository interface instead",
	"github.com/leadkart/leadkart-go/internal/platform/adapters":    "concrete adapter package leaks DB driver + sqlc; handlers must accept domain repository interfaces per Cheney",
	"github.com/jackc/pgx/v5":                                       "pgx driver is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgxpool":                               "pgxpool is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgtype":                                "pgtype is a sqlc/driver concern; should never appear in app/ — domain VOs carry strong types",
}

// TestArch_AppDoesNotImportForbidden asserts no non-test file under
// internal/platform/app/ imports a forbidden substrate path.
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
		t.Logf("BOUNDARY VIOLATIONS — %d forbidden imports in internal/platform/app/", len(violations))
		t.Logf("Per ADR 0047: app/ must depend on domain + platform interfaces, never on")
		t.Logf("the persistence substrate or its sqlc-generated row types.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s\n  why blocked: %s", v.file, v.imp, v.reason)
		}
	}
}
