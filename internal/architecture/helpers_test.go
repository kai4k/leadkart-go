// helpers_test.go — shared utilities for the fitness-function suite.

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

// repoRoot returns the repo root by walking up from this file until go.mod is found.
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

// modulesUnderInternal returns every bounded-context module dir under internal/,
// excluding "common" and "architecture".
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
// are included when includeTests=true. Generated files and
// //go:build integration non-test files are skipped. Silently returns
// if root does not exist.
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
		head := src
		if len(head) > 4096 {
			head = head[:4096]
		}
		if strings.Contains(string(head), "Code generated") &&
			strings.Contains(string(head), "DO NOT EDIT") {
			return nil
		}
		if !includeTests && strings.Contains(string(head), "//go:build integration") {
			return nil
		}
		fn(path, src)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// walkFilesByExt invokes fn for every file under root whose name ends in ext.
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

// parseImports returns the import paths in path. Records parse errors against t
// but does not fail (callers may continue walking).
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

// parseFile returns the AST + FileSet for path, fataling on parse error.
func parseFile(t *testing.T, path string, src []byte) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// readDirSafe wraps os.ReadDir, returning the error for the caller to handle.
func readDirSafe(dir string) ([]os.DirEntry, error) {
	return os.ReadDir(dir)
}

// readFileBytes wraps os.ReadFile.
func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// stripGoComments removes // and /* */ comments, preserving newlines for
// line-number arithmetic.
func stripGoComments(s string) string {
	blockRE := regexp.MustCompile(`(?s)/\*.*?\*/`)
	s = blockRE.ReplaceAllStringFunc(s, func(m string) string {
		return strings.Repeat("\n", strings.Count(m, "\n"))
	})
	lineRE := regexp.MustCompile(`//[^\n]*`)
	s = lineRE.ReplaceAllString(s, "")
	return s
}

// stripSQLComments removes -- and /* */ comments from a SQL string.
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

// callName returns the trailing identifier of a call expression.
func callName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// callPkgAndName returns (pkg, name) for `pkg.Func` calls, ("", name) for
// bare idents, and ("","") otherwise.
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

// typeName returns the name of an Ident-typed expression, or "<complex-type>".
func typeName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<complex-type>"
}

// isHandlerReceiver reports whether the receiver type ends in "Handler".
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

// returnsPointerAndError reports whether the result list is exactly (*T, error).
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

// pathToSlash normalises path separators to forward slashes.
func pathToSlash(p string) string { return filepath.ToSlash(p) }

// hasArchTestDirective reports whether lineText contains the given arch-test: directive.
func hasArchTestDirective(lineText, directive string) bool {
	return strings.Contains(lineText, "// arch-test:"+directive) ||
		strings.Contains(lineText, "//arch-test:"+directive)
}
