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
	"regexp"
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

// apiSpecPath returns <repoRoot>/api/openapi.yaml.
func apiSpecPath(t *testing.T) string {
	return filepath.Join(repoRoot(t), "api", "openapi.yaml")
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
//
// Silently returns if root does not exist — module-shape arch tests
// often probe optional subdirectories (e.g. ports/subscribers/ may
// not exist for read-only modules).
func walkGoFiles(t *testing.T, root string, includeTests bool, fn func(path string, src []byte)) {
	t.Helper()
	if _, err := os.Stat(root); err != nil {
		return
	}
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

// walkFilesByExt invokes fn for every file under root whose name ends
// in ext. Skips directories silently if root does not exist.
func walkFilesByExt(t *testing.T, root, ext string, fn func(path string, src []byte)) {
	t.Helper()
	if _, err := os.Stat(root); err != nil {
		return
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ext) {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
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

// stripGoComments removes // line comments + /* */ block comments
// while preserving newlines (so line-number arithmetic stays correct).
// Sufficient for go-fmt'd source where comment delimiters are never
// load-bearing inside string literals.
func stripGoComments(s string) string {
	blockRE := regexp.MustCompile(`(?s)/\*.*?\*/`)
	s = blockRE.ReplaceAllStringFunc(s, func(m string) string {
		return strings.Repeat("\n", strings.Count(m, "\n"))
	})
	lineRE := regexp.MustCompile(`//[^\n]*`)
	s = lineRE.ReplaceAllString(s, "")
	return s
}

// stripSQLComments removes -- line comments + /* */ block comments
// from a SQL source string.
func stripSQLComments(s string) string {
	blockRE := regexp.MustCompile(`(?s)/\*.*?\*/`)
	s = blockRE.ReplaceAllString(s, "")
	lineRE := regexp.MustCompile(`--[^\n]*`)
	s = lineRE.ReplaceAllString(s, "")
	return s
}

// readLine returns the 1-indexed Nth line of src, or "" if out of range.
func readLine(src string, n int) string {
	if n < 1 {
		return ""
	}
	cur := 1
	start := 0
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			if cur == n {
				return src[start:i]
			}
			cur++
			start = i + 1
		}
	}
	if cur == n {
		return src[start:]
	}
	return ""
}

// callName returns the trailing identifier of a function-call
// expression, supporting `pkg.Func`, `recv.Method`, and bare `Func`.
func callName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// callPkgAndName returns the package selector + name for `pkg.Func`
// call expressions, or ("", name) for bare Idents. Returns ("","")
// when the call isn't a simple selector / ident.
func callPkgAndName(e ast.Expr) (pkg, name string) {
	switch x := e.(type) {
	case *ast.Ident:
		return "", x.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name, x.Sel.Name
		}
	}
	return "", ""
}

// typeName returns the textual representation of an Ident-typed
// expression (for use in error messages).
func typeName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<complex-type>"
}

// isHandlerReceiver reports whether a FuncDecl receiver names a
// type ending in "Handler" (e.g. *LoginHandler, ListSessionsHandler).
// Both pointer + value receivers are accepted.
func isHandlerReceiver(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) == 0 {
		return false
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	if !ok {
		return false
	}
	return strings.HasSuffix(id.Name, "Handler")
}

// returnsPointerAndError reports whether the function declaration's
// result list is exactly `(*T, error)` for some T.
func returnsPointerAndError(fd *ast.FuncDecl) bool {
	if fd.Type.Results == nil || len(fd.Type.Results.List) != 2 {
		return false
	}
	if _, ok := fd.Type.Results.List[0].Type.(*ast.StarExpr); !ok {
		return false
	}
	id, ok := fd.Type.Results.List[1].Type.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "error"
}

// pathToSlash normalises Windows-style backslashes to forward slashes
// for path-matching predicates.
func pathToSlash(p string) string { return filepath.ToSlash(p) }

// hasArchTestNolint returns true when the comment text at `pos` (and
// the trailing comment on the same line) contains an `arch-test:`
// opt-out directive. We use specific directive prefixes (vs a blanket
// nolint) so each opt-out is intentional and grep-discoverable.
func hasArchTestDirective(lineText, directive string) bool {
	return strings.Contains(lineText, "// arch-test:"+directive) ||
		strings.Contains(lineText, "//arch-test:"+directive)
}
