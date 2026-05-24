// helpers_test.go — shared utilities for the fitness-function suite.
//
// Kept package-private to the architecture_test package so each
// TestArch_* test can focus on its rule + failure message, not on
// directory walking + AST parsing boilerplate.

package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot returns the absolute path to the repository root by walking
// up from this test file until it finds go.mod. The architecture
// package sits at internal/architecture/, so go.mod is two ".." up.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(here)
	for range 10 { // safety bound: stop after 10 parents
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root from %s", here)
	return ""
}

// internalDir returns <repoRoot>/internal.
func internalDir(t *testing.T) string {
	return filepath.Join(repoRoot(t), "internal")
}

// migrationsDir returns <repoRoot>/migrations.
func migrationsDir(t *testing.T) string {
	return filepath.Join(repoRoot(t), "migrations")
}

// adrDir returns <repoRoot>/docs/adr.
func adrDir(t *testing.T) string {
	return filepath.Join(repoRoot(t), "docs", "adr")
}

// modulesUnderInternal returns the names of every bounded-context
// module dir under internal/ that is NOT "common" or "architecture"
// (the architecture package + the shared kernel are not modules).
func modulesUnderInternal(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(internalDir(t))
	if err != nil {
		t.Fatalf("read internal/: %v", err)
	}
	var mods []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "common" || name == "architecture" {
			continue
		}
		mods = append(mods, name)
	}
	return mods
}

// walkGoFiles invokes fn for every .go file under root. _test.go files
// are included when includeTests=true. Generated files (matching the
// "// Code generated" canonical first-line marker) are always skipped.
func walkGoFiles(t *testing.T, root string, includeTests bool, fn func(path string, src []byte)) {
	t.Helper()
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
		if !includeTests && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
			return nil
		}
		// Skip generated files. The canon marker per
		// https://pkg.go.dev/cmd/go#hdr-Generate_Go_files is a
		// comment matching `^// Code generated .* DO NOT EDIT\.$`
		// on a non-blank line before the package clause.
		head := src
		if len(head) > 4096 {
			head = head[:4096]
		}
		if strings.Contains(string(head), "Code generated") &&
			strings.Contains(string(head), "DO NOT EDIT") {
			return nil
		}
		fn(path, src)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// parseImports returns the import paths in the Go source at path. On
// parse error, returns nil + records the error against t (does NOT
// fail the test — caller may want to keep walking).
func parseImports(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
	if err != nil {
		t.Errorf("parse %s: %v", path, err)
		return nil
	}
	out := make([]string, 0, len(f.Imports))
	for _, im := range f.Imports {
		out = append(out, strings.Trim(im.Path.Value, `"`))
	}
	return out
}

// parseFile returns the AST + FileSet for path. On parse error, t.Fatal.
func parseFile(t *testing.T, path string, src []byte) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// readDirSafe wraps os.ReadDir but returns the underlying err so the
// caller may decide to skip non-existent directories silently.
func readDirSafe(dir string) ([]os.DirEntry, error) {
	return os.ReadDir(dir)
}

// readFileBytes wraps os.ReadFile for fixture/test consumption.
func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}
