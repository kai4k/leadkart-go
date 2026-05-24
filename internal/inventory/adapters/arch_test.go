// arch_test.go — boundary discipline for the Inventory module's
// adapters/ layer. Companion to internal/inventory/app/arch_test.go
// (ADR 0047) + ADR 0061 amendment 1 (H5).
//
// adapters/ may legitimately import pgx + sqlc-generated row types
// (that's its job — driver + persistence). It MUST NOT, however,
// reach into ANOTHER module's bounded context. Per CLAUDE.md
// "Architecture rule 1: modules NEVER reference each other's
// domain/app/ports/adapters" — the inventory adapter layer was
// caught (round-1 finding H5) importing
// `internal/identity/app/actclaim`. Fix-pass duplicated actclaim
// locally; this gate prevents the regression.
//
// Why a SEPARATE arch test for adapters/ (not just one in app/):
// app/'s test scopes to app/ (`filepath.Clean(".")` walks the test
// file's directory tree). A ban on identity imports declared there
// would not catch an adapter-layer regression. Per the discipline of
// CLAUDE.md "drift = finding" we add the gate at the layer the
// finding actually surfaced.

package adapters_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenInventoryAdapterImports lists cross-module import paths
// that inventory adapters/ MUST NOT depend on. Identity DOMAIN types
// (tenant.ID, membership.ID) are allowed — they're VO-shaped strings
// flowing across the bounded-context seam on integration events; the
// adapter/app/ports/db layers of OTHER modules are not.
var forbiddenInventoryAdapterImports = map[string]string{
	"github.com/leadkart/leadkart-go/internal/identity/adapters":     "cross-module: inventory adapters/ MUST NOT import another module's concrete adapters (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db":  "cross-module: inventory adapters/ MUST NOT import another module's sqlc-generated row types (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/app":          "cross-module: inventory adapters/ MUST NOT import another module's app layer (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/app/actclaim": "cross-module: actclaim is duplicated locally per ADR 0061 amendment 1 (H5); identity's actclaim stays in identity's bounded context",
	"github.com/leadkart/leadkart-go/internal/identity/ports":        "cross-module: inventory adapters/ MUST NOT import another module's HTTP ports / subscribers (CLAUDE.md §Architecture rule 1)",
}

// TestArch_AdaptersDoNotImportForbiddenCrossModule walks every
// non-test .go file under internal/inventory/adapters/ (test files
// are exempt — integration-test fixtures legitimately use identity
// adapters for seedTenant). Production code MUST stay within the
// inventory bounded context.
func TestArch_AdaptersDoNotImportForbiddenCrossModule(t *testing.T) {
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
			// Integration-test fixtures legitimately use identity
			// adapters (seedTenant), so test files are exempt.
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, im := range f.Imports {
			imPath := strings.Trim(im.Path.Value, `"`)
			if reason, bad := forbiddenInventoryAdapterImports[imPath]; bad {
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
		t.Logf("CROSS-MODULE BOUNDARY VIOLATIONS — %d forbidden imports in internal/inventory/adapters/", len(violations))
		t.Logf("Per CLAUDE.md \"Architecture rule 1\" + ADR 0061 amendment 1 (H5):")
		t.Logf("adapters/ may reach the persistence substrate but NOT another module's bounded context.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s\n  why blocked: %s", v.file, v.imp, v.reason)
		}
	}
}
