// visibility_arch_test.go — Go's analog of ".NET handlers internal by
// default". Go has no `internal` access modifier on declarations; the
// idiom is "unexported unless used across package boundaries", backed by
// the internal/ directory for module-level encapsulation. An exported
// symbol with zero cross-package use is a gratuitous export — widen-the-
// surface-for-nothing — and should be lowercased.
//
// This gate targets the packages where leaked helpers actually hide:
// app/command, app/query, and adapters (the generated db/ subpkg is
// skipped). It flags exported FREE FUNCTIONS (not methods, not New*
// constructors, not types — those are the legitimate API surface) that no
// file OUTSIDE their own package references as `.Name(`.
//
// The textual `.Name(` usage scan deliberately over-counts (a like-named
// method on any type reads as "used"), so the gate errs toward NOT
// flagging — false negatives, never false positives. A symbol exported for
// reflection or passed as a value (no call paren) can carry
// `// arch-test:exported-for-wiring` to opt out.
//
// Scope: production — only production exports are candidates (collected with
// includeTests=false); an exported helper used solely by tests is not a
// surface leak worth lowercasing. The usage scan in pass 2 DOES include
// tests, so a test-only consumer still counts as legitimate external use.
//
// arch-test:no-negative-fixture — a committed testdata fixture would be a
// genuinely-dead exported func, which `unused`/review would flag anyway;
// the RED→GREEN proof is the mutation test (export an unused helper → RED).
//
// arch-test:no-synctest — purely-static AST + text analysis.
package architecture_test

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

func TestArch_NoGratuitousExports(t *testing.T) {
	t.Parallel()

	const optOut = "arch-test:exported-for-wiring"

	type candidate struct {
		name string
		file string
		line int
		dir  string
	}
	var candidates []candidate

	isTargetPkg := func(slash string) bool {
		return strings.Contains(slash, "/app/command/") ||
			strings.Contains(slash, "/app/query/") ||
			(strings.Contains(slash, "/adapters/") && !strings.Contains(slash, "/adapters/db/"))
	}

	// Pass 1 — collect exported free-function candidates.
	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTargetPkg(slash) {
			return
		}
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil { // methods are part of a type's surface
				continue
			}
			if !fn.Name.IsExported() || strings.HasPrefix(fn.Name.Name, "New") {
				continue
			}
			if fn.Doc != nil && strings.Contains(fn.Doc.Text(), optOut) {
				continue
			}
			candidates = append(candidates, candidate{
				name: fn.Name.Name,
				file: slash,
				line: fset.Position(fn.Pos()).Line,
				dir:  pathToSlash(filepath.Dir(path)),
			})
		}
	})

	if len(candidates) == 0 {
		return
	}

	// Pass 2 — for each candidate name, which package dirs reference it as
	// `.Name(`? Scan all of internal/ AND cmd/ (incl. tests — a test using
	// the symbol is legitimate external use). Generated files are skipped by
	// walkGoFiles.
	usedFromDir := make(map[string]map[string]bool, len(candidates))
	for _, c := range candidates {
		usedFromDir[c.name] = map[string]bool{}
	}
	scan := func(path string, src []byte) {
		dir := pathToSlash(filepath.Dir(path))
		text := string(src)
		for name := range usedFromDir {
			if strings.Contains(text, "."+name+"(") {
				usedFromDir[name][dir] = true
			}
		}
	}
	walkGoFiles(t, internalDir(t), true, scan)
	walkGoFiles(t, filepath.Join(repoRoot(t), "cmd"), true, scan)

	var dead []candidate
	for _, c := range candidates {
		external := false
		for dir := range usedFromDir[c.name] {
			if dir != c.dir {
				external = true
				break
			}
		}
		if !external {
			dead = append(dead, c)
		}
	}

	if len(dead) > 0 {
		t.Errorf("%d exported function(s) with no cross-package use — unexport (Go 'internal by default'), or mark `// %s`:", len(dead), optOut)
		for _, c := range dead {
			t.Errorf("  %s:%d — func %s is used only within its own package", c.file, c.line, c.name)
		}
	}
}
