// arch_test.go — architectural boundary discipline as a CI gate for the
// Inventory module's app/ layer. Mirror of internal/identity/app/arch_test.go
// per ADR 0047.
//
// The app/ layer is a pure-Go region that depends on domain interfaces +
// a handful of cross-cutting infra interfaces (pg.UnitOfWork). It MUST
// NOT depend on:
//   - github.com/leadkart/leadkart-go/internal/inventory/adapters/db
//     (sqlc-generated row types — DB-shape leaks into the app layer)
//   - github.com/jackc/pgx/v5 / pgx/v5/pgxpool / pgx/v5/pgtype
//     (DB driver — leaks the substrate)
//   - github.com/leadkart/leadkart-go/internal/inventory/adapters
//     (concrete adapter package — handlers must accept INTERFACES)
//
// Drift = finding: fix the import (route through an interface) — do
// NOT add the import to the allowlist without an ADR amendment.
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
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db": "sqlc-generated row types leak DB shape into the application layer; use a domain-side Reader/Repository interface instead",
	"github.com/leadkart/leadkart-go/internal/inventory/adapters":    "concrete adapter package leaks DB driver + sqlc; handlers must accept domain repository interfaces per Cheney",
	"github.com/jackc/pgx/v5":                                        "pgx driver is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgxpool":                                "pgxpool is a substrate concern; use pg.UnitOfWork or domain interfaces",
	"github.com/jackc/pgx/v5/pgtype":                                 "pgtype is a sqlc/driver concern; should never appear in app/ — domain VOs carry strong types",

	// Per CLAUDE.md "Architecture rule 1: modules NEVER reference each
	// other's domain/app/ports/adapters" — inventory app/ MUST NOT
	// reach into identity's substrate either. Identity domain types
	// (tenant.ID, membership.ID) are allowed because they're VO-shaped
	// strings carried across module boundaries on integration events;
	// adapters + app + db packages are NOT.
	"github.com/leadkart/leadkart-go/internal/identity/adapters":    "cross-module: inventory app/ MUST NOT import another module's concrete adapters (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db": "cross-module: inventory app/ MUST NOT import another module's sqlc-generated row types (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/app":         "cross-module: inventory app/ MUST NOT import another module's application layer; use events on the bus (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/app/actclaim": "cross-module: actclaim is duplicated locally per ADR 0061 amendment 1 (H5); identity's actclaim stays in identity's bounded context",
	"github.com/leadkart/leadkart-go/internal/identity/ports":       "cross-module: inventory app/ MUST NOT import another module's HTTP ports / subscribers (CLAUDE.md §Architecture rule 1)",
}

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
		t.Logf("BOUNDARY VIOLATIONS — %d forbidden imports in internal/inventory/app/", len(violations))
		t.Logf("Per ADR 0047: app/ must depend on domain + platform interfaces, never on")
		t.Logf("the persistence substrate or its sqlc-generated row types.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s\n  why blocked: %s", v.file, v.imp, v.reason)
		}
	}
}
