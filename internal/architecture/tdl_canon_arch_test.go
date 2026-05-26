// tdl_canon_arch_test.go — Fitness functions enforcing the TDL
// (ThreeDotsLabs) test-pyramid canon for domain aggregates.
//
// THE INVARIANTS:
//
//   1. Every domain aggregate exposing a Repository interface MUST
//      have a co-located <aggregate>test/ package with a
//      FakeRepository type. Per TDL Wild Workouts canon: the fake
//      lives next to the contract it implements; consumers (tests)
//      import the fake by name.
//
//   2. Every <aggregate>test.FakeRepository MUST declare a compile-
//      time interface conformance gate (`var _ X.Repository =
//      (*FakeRepository)(nil)`). Without it, interface drift goes
//      undetected at build time; tests would only fail at runtime
//      on the first method call against the missing/renamed signature.
//
//   3. Every <aggregate>test/ package MUST NOT import `sync`. Domain
//      subtree is concurrency-free per Bryan Mills GopherCon canon +
//      already enforced by TestArch_NoGoroutinesInDomain — this
//      duplicates the rule for the test packages co-located with the
//      domain. The single-test-owner pattern means each test creates
//      its own fake; no shared mutable state across tests.
//
// CANON SOURCES:
//   - ThreeDotsLabs "Go with the Domain" ch. 8: fakes (faithful impls)
//     over mocks (call-pattern coupling)
//   - Wild Workouts repo: <aggregate>test/ co-location pattern
//   - Cheney "accept interfaces, return structs": the consumer
//     (app/command/) defines the Repository interface; adapters
//     implement; tests substitute via the fake
//   - Khorikov "Unit Testing Principles" §5: prefer state-based
//     assertions over interaction-based (mock) assertions
//
// FAILURE MODES CAUGHT:
//
//   - A new aggregate ships without its fake → integration-only test
//     coverage → slow CI + missing unit-test discipline
//   - Repository interface evolves without the fake being updated →
//     runtime breakage on first .Add / .GetByID after the rename
//   - A future engineer adds sync.RWMutex to a fake → trips
//     TestArch_NoGoroutinesInDomain at next CI run OR masks a real
//     concurrency bug in domain logic
//
// arch-test:parallel-safe-file — every Test* below uses only file
//   reads + AST walks; no shared mutable state.

package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestArch_EveryRepositoryHasFake asserts every domain aggregate
// that defines a Repository interface in its repository.go file has
// a co-located <aggregate>test/fake_repository.go that exports a
// FakeRepository type.
//
// arch-test:no-negative-fixture — the assertion target is the live
// internal/<module>/domain/ tree. A synthetic fixture would defeat
// the "guards the actual codebase" guarantee.
func TestArch_EveryRepositoryHasFake(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type missing struct {
		repoFile  string
		expectDir string
	}
	var violations []missing

	// Find every <module>/domain/<aggregate>/repository.go file.
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) != "repository.go" {
			return nil
		}
		// Must be under <module>/domain/<aggregate>/repository.go.
		// Reject e.g. internal/common/pg/repository.go (substrate-layer).
		slash := pathToSlash(path)
		if !strings.Contains(slash, "/domain/") {
			return nil
		}
		// The file must declare a Repository interface (some
		// repository.go files declare only sentinel errors / value
		// objects without a Repository interface).
		if !declaresRepositoryInterface(t, path) {
			return nil
		}
		aggregateDir := filepath.Dir(path)
		aggregateName := filepath.Base(aggregateDir)
		expectFakeDir := filepath.Join(aggregateDir, aggregateName+"test")
		expectFakeFile := filepath.Join(expectFakeDir, "fake_repository.go")
		if _, err := os.Stat(expectFakeFile); err == nil {
			return nil
		}
		violations = append(violations, missing{
			repoFile:  slash,
			expectDir: pathToSlash(expectFakeDir),
		})
		return nil
	})

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool { return violations[i].repoFile < violations[j].repoFile })
	t.Errorf("%d domain aggregate(s) with a Repository interface but no co-located <aggregate>test FakeRepository — required by TDL canon:", len(violations))
	t.Logf("Per TDL Wild Workouts: every Repository interface gets a faithful")
	t.Logf("FakeRepository in a sibling test package. Reference:")
	t.Logf("  internal/identity/domain/role/roletest/fake_repository.go")
	for _, v := range violations {
		t.Logf("  %s — expected %s/fake_repository.go", v.repoFile, v.expectDir)
	}
}

// TestArch_FakeRepositoryHasCompileGate asserts every
// <aggregate>test/fake_repository.go contains a compile-time interface
// conformance check of the form:
//
//	var _ <aggregate>.Repository = (*FakeRepository)(nil)
//
// Drift in the Repository interface (a method renamed, signature
// changed) breaks at build time before any test runs.
//
// arch-test:no-negative-fixture — same rationale as the sibling test.
func TestArch_FakeRepositoryHasCompileGate(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type missing struct {
		file string
	}
	var violations []missing

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) != "fake_repository.go" {
			return nil
		}
		slash := pathToSlash(path)
		if !strings.Contains(slash, "/domain/") || !strings.HasSuffix(filepath.Dir(slash), "test") {
			return nil
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // arch-test fixture path
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
			return nil
		}
		// Strict pattern match: assignment to the blank identifier
		// where the LHS is `<pkg>.Repository` and the RHS is a typed
		// `(*FakeRepository)(nil)` zero value.
		body := string(raw)
		if !strings.Contains(body, "Repository = (*FakeRepository)(nil)") {
			violations = append(violations, missing{file: slash})
		}
		return nil
	})

	if len(violations) == 0 {
		return
	}

	t.Errorf("%d fake_repository.go file(s) missing the compile-time interface-conformance gate:", len(violations))
	t.Logf("Required pattern (TDL canon):")
	t.Logf("  var _ <aggregate>.Repository = (*FakeRepository)(nil)")
	t.Logf("Without this, interface drift goes undetected at build time.")
	for _, v := range violations {
		t.Logf("  %s", v.file)
	}
}

// TestArch_FakeRepositoryHasNoSync asserts that <aggregate>test/
// packages don't import the `sync` standard-library package. Domain
// subtree is concurrency-free by canon (Bryan Mills GopherCon 2018);
// the single-test-owner pattern means each test creates its own fake
// instance, so no shared mutable state exists across goroutines.
//
// Adding sync.RWMutex to a fake hides a real concurrency bug in the
// domain code being tested OR introduces a false-positive concurrency
// safety claim. Neither is acceptable.
//
// arch-test:no-negative-fixture — same rationale as the sibling test.
func TestArch_FakeRepositoryHasNoSync(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type violation struct {
		file string
		line int
	}
	var violations []violation

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		slash := pathToSlash(path)
		if !strings.Contains(slash, "/domain/") {
			return nil
		}
		// Must be in an <aggregate>test/ package, not the aggregate
		// itself (the aggregate's own no-sync rule is enforced by
		// TestArch_NoGoroutinesInDomain).
		dir := filepath.Dir(slash)
		if !strings.HasSuffix(dir, "test") {
			return nil
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // arch-test fixture path
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, raw, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, imp := range f.Imports {
			pathLit := strings.Trim(imp.Path.Value, `"`)
			if pathLit == "sync" {
				violations = append(violations, violation{
					file: slash,
					line: fset.Position(imp.Pos()).Line,
				})
			}
		}
		return nil
	})

	if len(violations) == 0 {
		return
	}

	t.Errorf("%d <aggregate>test/ file(s) import `sync` — forbidden per TDL canon:", len(violations))
	t.Logf("Single-test-owner pattern: each test creates its OWN fake")
	t.Logf("instance via NewFakeRepository(). No shared mutable state =>")
	t.Logf("no sync primitives needed. Adding sync.Mutex hides real")
	t.Logf("concurrency bugs in the SUT or claims false safety.")
	t.Logf("Reference: internal/identity/domain/role/roletest/fake_repository.go")
	for _, v := range violations {
		t.Logf("  %s:%d", v.file, v.line)
	}
}

// declaresRepositoryInterface reports whether the supplied repository.go
// file declares an interface type named "Repository". Some
// repository.go files in the codebase declare only sentinel errors
// or value objects without an actual Repository interface — those are
// skipped by the EveryRepositoryHasFake gate.
func declaresRepositoryInterface(t *testing.T, path string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Errorf("parse %s: %v", path, err)
		return false
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Repository" {
				continue
			}
			if _, ok := ts.Type.(*ast.InterfaceType); ok {
				return true
			}
		}
	}
	return false
}
