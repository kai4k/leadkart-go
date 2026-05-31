// arch_test.go — cross-module boundary gate for inventory/adapters.
// Companion to internal/inventory/app/arch_test.go (ADR 0047) +
// ADR 0061 amendment 1 (H5). Declared here, not in app/, because
// app/'s gate only walks app/'s subtree and would miss an
// adapter-layer regression (CLAUDE.md "drift = finding").

package adapters_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenInventoryAdapterImports lists cross-module paths banned in
// production adapter code. Identity domain VOs (tenant.ID,
// membership.ID) are allowed — other modules' adapters/app/ports are not.
var forbiddenInventoryAdapterImports = map[string]string{
	"github.com/leadkart/leadkart-go/internal/identity/adapters":    "cross-module: inventory adapters/ MUST NOT import another module's concrete adapters (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db": "cross-module: inventory adapters/ MUST NOT import another module's sqlc-generated row types (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/identity/app":         "cross-module: inventory adapters/ MUST NOT import another module's app layer (CLAUDE.md §Architecture rule 1)",
	"github.com/leadkart/leadkart-go/internal/common/actclaim":      "cross-module: actclaim is duplicated locally per ADR 0061 amendment 1 (H5); identity's actclaim stays in identity's bounded context",
	"github.com/leadkart/leadkart-go/internal/identity/ports":       "cross-module: inventory adapters/ MUST NOT import another module's HTTP ports / subscribers (CLAUDE.md §Architecture rule 1)",
}

// TestArch_AdaptersDoNotImportForbiddenCrossModule walks non-test
// .go files under internal/inventory/adapters/ and fails on any
// forbidden cross-module import. Test files are exempt — integration
// fixtures legitimately use identity adapters for seedTenant.
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
			// Test files are exempt; integration fixtures use identity adapters.
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
