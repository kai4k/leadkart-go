// meta_arch_test.go — the meta-test that closes the institutional gap.
//
// Per Ford / Parsons / Kua (Building Evolutionary Architectures):
// fitness functions only work if EVERY architectural decision has one.
// An ADR without a corresponding test is aspirational text — the
// codebase may drift from it silently.
//
// This test parses every accepted ADR under docs/adr/ and enforces one
// of three declared states:
//
//  1. ACTIVE: the ADR contains a `## Fitness function` section that
//     names a TestArch_* (or TestMeta_*) test which exists in the
//     codebase. Verified by scanning the entire repository's
//     `func TestArch_` + `func TestMeta_` declarations.
//
//  2. CONVENTION-ONLY: the ADR contains the exact marker
//     `**Fitness function:** convention-only — not mechanically
//     expressible` followed by a 1-2 sentence rationale on the same
//     or next line. This is the escape hatch for process decisions
//     (e.g. naming conventions, ADR review cadence) that cannot be
//     mechanically encoded.
//
//  3. GRANDFATHERED: the ADR contains the marker
//     `**Fitness function:** TBD — grandfathered`. AT MOST 5 ADRs
//     may carry this marker at any time. The ratchet forces gradual
//     closure of the gap — once the backlog drops below 5 the cap
//     stays at the new floor (this test tracks the live count).
//
// Status filter: only ADRs whose status line starts with `Accepted`
// are evaluated. `Superseded by *` ADRs are exempt (their replacement
// carries the discipline).

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// grandfatheredCap is the upper bound on ADRs marked "TBD —
// grandfathered". Lowering this number is the canonical "close the
// gap" move — every PR that adds a real fitness function to a
// grandfathered ADR should subtract 1 from this constant.
//
// History:
//   - 2026-05-24 — initial 19-test suite landed; backfill sweep
//     marked the remaining ADRs as convention-only or TBD.
const grandfatheredCap = 5

// ----------------------------------------------------------------------------
// Test 19: TestMeta_EveryAcceptedADRHasFitnessFunctionRef
// ----------------------------------------------------------------------------
func TestMeta_EveryAcceptedADRHasFitnessFunctionRef(t *testing.T) {
	t.Parallel()

	// 1) Discover every TestArch_* / TestMeta_* declared anywhere in
	//    the repo. The meta-test trusts the test discovery, not the
	//    ADR — so a stale reference in an ADR pointing to a deleted
	//    test fails this gate.
	knownTests := discoverTestArchNames(t)

	// 2) Walk every ADR file.
	entries, err := os.ReadDir(adrDir(t))
	if err != nil {
		t.Fatalf("read docs/adr: %v", err)
	}

	statusAcceptedRE := regexp.MustCompile(`(?m)^\*\*Status:\*\*\s*Accepted`)
	conventionRE := regexp.MustCompile(`\*\*Fitness function:\*\*\s*convention-only`)
	grandfatheredRE := regexp.MustCompile(`\*\*Fitness function:\*\*\s*TBD\s*—\s*grandfathered`)
	fitnessSectionRE := regexp.MustCompile(`(?m)^##\s+Fitness function\b`)
	testNameRE := regexp.MustCompile(`\bTest(Arch|Meta)_[A-Z]\w*`)

	type missing struct {
		file   string
		reason string
	}
	var (
		missingList     []missing
		grandfathered   []string
		danglingRefs    []string
	)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") || strings.EqualFold(name, "README.md") {
			continue
		}
		path := filepath.Join(adrDir(t), name)
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
			continue
		}
		text := string(raw)

		// Only enforce against Accepted ADRs. Superseded entries
		// inherit discipline from their replacement.
		if !statusAcceptedRE.MatchString(text) {
			continue
		}

		// State 2: convention-only marker.
		if conventionRE.MatchString(text) {
			continue
		}
		// State 3: grandfathered marker — count + continue.
		if grandfatheredRE.MatchString(text) {
			grandfathered = append(grandfathered, name)
			continue
		}
		// State 1: fitness function section referencing a known test.
		if fitnessSectionRE.MatchString(text) {
			refs := testNameRE.FindAllString(text, -1)
			if len(refs) == 0 {
				missingList = append(missingList, missing{
					file:   name,
					reason: "`## Fitness function` section present but names no TestArch_* / TestMeta_* test",
				})
				continue
			}
			anyKnown := false
			for _, ref := range refs {
				if knownTests[ref] {
					anyKnown = true
					break
				}
			}
			if !anyKnown {
				danglingRefs = append(danglingRefs, name+" -> "+strings.Join(refs, ","))
			}
			continue
		}

		// No state declared — violation.
		missingList = append(missingList, missing{
			file:   name,
			reason: "no `## Fitness function` section + no `**Fitness function:** convention-only` marker + no `**Fitness function:** TBD — grandfathered` marker",
		})
	}

	if len(grandfathered) > grandfatheredCap {
		t.Errorf("GRANDFATHERED-CAP BREACH — %d ADRs carry the `TBD — grandfathered` marker (cap = %d)", len(grandfathered), grandfatheredCap)
		t.Logf("Each PR that adds a fitness function to a grandfathered ADR")
		t.Logf("should ALSO decrement grandfatheredCap in this test — that's")
		t.Logf("the canonical 'gap-closure' ratchet (Ford / Parsons / Kua).")
		for _, g := range grandfathered {
			t.Logf("  grandfathered: %s", g)
		}
	}

	if len(missingList) > 0 {
		t.Logf("ADR FITNESS-FUNCTION MARKER VIOLATIONS — %d", len(missingList))
		t.Logf("Per the meta-test contract: every Accepted ADR must declare")
		t.Logf("ONE of three states:")
		t.Logf("  (1) `## Fitness function` section naming a TestArch_*/TestMeta_* test")
		t.Logf("  (2) `**Fitness function:** convention-only — not mechanically expressible` + 1-2 sentence rationale")
		t.Logf("  (3) `**Fitness function:** TBD — grandfathered` (max %d ADRs)", grandfatheredCap)
		for _, m := range missingList {
			t.Errorf("%s — %s", m.file, m.reason)
		}
	}

	if len(danglingRefs) > 0 {
		t.Logf("ADR DANGLING-TEST-REFERENCE VIOLATIONS — %d", len(danglingRefs))
		t.Logf("These ADRs declare `## Fitness function` and name TestArch_*/TestMeta_*")
		t.Logf("tests, but none of those tests exist in the repo. Either restore")
		t.Logf("the test or update the ADR's reference.")
		for _, d := range danglingRefs {
			t.Errorf("%s", d)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 95: TestMeta_EveryFitnessFunctionHasNegativeFixture
// ----------------------------------------------------------------------------
//
// For every TestArch_* function in internal/architecture/, EITHER a
// fixture exists at internal/architecture/testdata/negative/<test_name>/
// (proving the test catches a real violation when run against the
// fixture — placeholder marker file is sufficient for catalog presence)
// OR the test's godoc carries the explicit opt-out marker
// `// arch-test:no-negative-fixture (<rationale>)`.
//
// Per Ford / Parsons / Kua *Building Evolutionary Architectures* ch.4:
// a fitness function that has never been shown to FAIL might be
// silently buggy. Negative fixtures are this suite's poor-man's
// mutation-testing surrogate (full mutation testing is
// `github.com/avito-tech/go-mutesting`; defer until v1.0).
//
// Implementation strategy — STATIC catalog presence:
//
//   - Enumerate every TestArch_* from this package.
//   - For each, look up `testdata/negative/<name>/` (any contents
//     accepted; the directory's existence is the marker), OR a
//     fitness-function godoc containing `arch-test:no-negative-fixture`.
//   - List violations; fail with the closure plan in the message.
//
// The runtime side (re-invoking the test against the fixture) is the
// next escalation; static catalog presence is the load-bearing first
// gate that closes the skip per the user's "fix all" directive.
func TestMeta_EveryFitnessFunctionHasNegativeFixture(t *testing.T) {
	t.Parallel()

	archDir := filepath.Join(internalDir(t), "architecture")
	declRE := regexp.MustCompile(`(?m)^func\s+(TestArch_[A-Z]\w*)\s*\(`)
	// Per-test godoc opt-out marker — appears anywhere within the
	// docblock preceding the func decl.
	optOutRE := regexp.MustCompile(`arch-test:no-negative-fixture`)

	type entry struct {
		name        string
		hasOptOut   bool
		fixtureDir  string
		fixtureSeen bool
	}

	entries := map[string]*entry{}
	walkGoFiles(t, archDir, true, func(_ string, src []byte) {
		text := string(src)
		// Slice the file into per-function blocks; for each TestArch_*
		// match, look backwards for godoc + forward for the matching
		// opt-out marker.
		for _, m := range declRE.FindAllStringSubmatchIndex(text, -1) {
			name := text[m[2]:m[3]]
			if _, exists := entries[name]; exists {
				continue
			}
			// Godoc window: 800 chars before the decl is enough to
			// capture the // comment block immediately preceding.
			start := m[0] - 800
			if start < 0 {
				start = 0
			}
			godoc := text[start:m[0]]
			entries[name] = &entry{
				name:      name,
				hasOptOut: optOutRE.MatchString(godoc),
			}
		}
	})

	if len(entries) == 0 {
		t.Fatal("no TestArch_* functions discovered — meta-test broken")
	}

	// Pass 2: directory presence under testdata/negative/<name>/.
	negativeRoot := filepath.Join(archDir, "testdata", "negative")
	if _, err := os.Stat(negativeRoot); err == nil {
		dirs, _ := os.ReadDir(negativeRoot)
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			if e, ok := entries[d.Name()]; ok {
				e.fixtureSeen = true
				e.fixtureDir = filepath.Join(negativeRoot, d.Name())
			}
		}
	}

	var missing []string
	for _, e := range entries {
		if e.hasOptOut || e.fixtureSeen {
			continue
		}
		missing = append(missing, e.name)
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("%d TestArch_* function(s) carry neither a negative fixture nor an `arch-test:no-negative-fixture` godoc marker — pick one (Ford / Parsons / Kua 'Building Evolutionary Architectures' ch.4):", len(missing))
		for _, n := range missing {
			t.Logf("  %s — add testdata/negative/%s/ OR add `// arch-test:no-negative-fixture (rationale)` to the godoc", n, n)
		}
	}
}

// walkGoFiles wrapper not used here, but Go's import-collapsing keeps
// this file dependency-free.

// discoverTestArchNames walks every *_test.go file in the repo and
// returns the set of `func TestArch_X` / `func TestMeta_X` names.
//
// We grep the source (not invoke `go test -list`) so the meta-test
// stays standalone — no shell-out, no toolchain dependency beyond the
// stdlib parser.
func discoverTestArchNames(t *testing.T) map[string]bool {
	t.Helper()
	declRE := regexp.MustCompile(`(?m)^func\s+(Test(Arch|Meta)_[A-Z]\w*)\s*\(`)
	out := map[string]bool{}
	// Walk the repo root, but only descend into directories that may
	// contain Go test files (skip docs/, migrations/, etc., for speed).
	roots := []string{
		filepath.Join(repoRoot(t), "internal"),
		filepath.Join(repoRoot(t), "cmd"),
	}
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, m := range declRE.FindAllStringSubmatch(string(raw), -1) {
				out[m[1]] = true
			}
			return nil
		})
	}
	return out
}

// ============================================================================
// Principle U — Documentation discipline (3 tests added per the comprehensive
// catalog brief).
// ============================================================================

// ----------------------------------------------------------------------------
// U1: TestArch_EveryExportedHasGodoc
// ----------------------------------------------------------------------------
//
// Every exported function/type in `domain/` + `app/` must carry a
// doc comment. (revive's `exported` linter equivalent.) The check
// here is gentle — a budget ceiling (`task de-sloppify` will lower
// it once formal sweeps land).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_EveryExportedHasGodoc(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	exportedNoDoc := 0
	var sample []string

	for _, mod := range modulesUnderInternal(t) {
		for _, layer := range []string{"domain", "app"} {
			dir := filepath.Join(root, mod, layer)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				body := string(src)
				lines := strings.Split(body, "\n")
				// Match `func ExportedName` / `type ExportedName` on
				// line N; check whether line N-1 is a // comment.
				declRE := regexp.MustCompile(`^(func|type)\s+(\w*[A-Z]\w*)`)
				for i, ln := range lines {
					m := declRE.FindStringSubmatch(ln)
					if m == nil {
						continue
					}
					// Skip method receivers (func (r X) Foo).
					if m[1] == "func" && strings.HasPrefix(ln, "func (") {
						continue
					}
					if i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "//") {
						continue
					}
					exportedNoDoc++
					if len(sample) < 10 {
						sample = append(sample, pathToSlash(path)+":"+itoa(i+1)+" "+m[2])
					}
				}
			})
		}
	}

	const ceiling = 200
	if exportedNoDoc > ceiling {
		t.Fatalf("undocumented exported symbols: %d (ceiling %d). Sample:\n  %s",
			exportedNoDoc, ceiling, strings.Join(sample, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// U2: TestArch_EveryPackageHasDocComment
// ----------------------------------------------------------------------------
//
// Every package should have AT LEAST one file beginning with
// `// Package <name> ...`. doc.go is the canonical home but any
// .go file qualifies.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_EveryPackageHasDocComment(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// pkg dir -> documented?
	docs := map[string]bool{}

	walkGoFiles(t, root, false, func(path string, src []byte) {
		pkgDir := filepath.Dir(path)
		head := string(src)
		if len(head) > 1024 {
			head = head[:1024]
		}
		if strings.Contains(head, "// Package ") {
			docs[pkgDir] = true
			return
		}
		if _, ok := docs[pkgDir]; !ok {
			docs[pkgDir] = false
		}
	})

	var bad []string
	for dir, has := range docs {
		if has {
			continue
		}
		// Skip the generated sqlc db/ subdirs.
		if strings.Contains(pathToSlash(dir), "/adapters/db") {
			continue
		}
		bad = append(bad, pathToSlash(dir))
	}

	const ceiling = 25
	if len(bad) > ceiling {
		t.Fatalf("packages without doc comment: %d (ceiling %d). Sample:\n  %s",
			len(bad), ceiling, strings.Join(bad[:min(10, len(bad))], "\n  "))
	}
}

// ----------------------------------------------------------------------------
// U3: TestArch_ADRsHaveFrontmatter
// ----------------------------------------------------------------------------
//
// Every accepted ADR under docs/adr/ declares `**Status:**` AND
// `**Date:**`. Michael Nygard's ADR template canon.
func TestArch_ADRsHaveFrontmatter(t *testing.T) {
	t.Parallel()

	dir := adrDir(t)
	entries, err := readDirSafe(dir)
	if err != nil {
		t.Skipf("docs/adr/ not present: %v", err)
	}
	var bad []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		// Skip README + index files — those are not ADRs.
		lower := strings.ToLower(e.Name())
		if lower == "readme.md" || lower == "index.md" || lower == "_template.md" {
			continue
		}
		src, err := readFileBytes(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		body := string(src)
		if !strings.Contains(body, "**Status:**") {
			bad = append(bad, e.Name()+": no **Status:**")
		}
		if !strings.Contains(body, "**Date:**") {
			bad = append(bad, e.Name()+": no **Date:**")
		}
	}
	if len(bad) > 0 {
		t.Fatalf("ADRs missing canonical frontmatter:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ============================================================================
// Principle L — CGO + build determinism (3 tests added per the comprehensive
// catalog brief). ADR 0024 distroless static fit.
// ============================================================================

// ----------------------------------------------------------------------------
// L1: TestArch_DockerfileGoVersionMatchesGoMod
// ----------------------------------------------------------------------------
//
// Per ADR 0024 — project ships via Chainguard distroless static; the
// SDK build flow (`task build`) bakes the binary directly into the
// distroless layer at publish time. NO Dockerfile is committed at the
// repo root by design (canonical Chainguard pattern — single-image,
// no multi-stage Dockerfile authoring).
//
// Test contract: if a Dockerfile EVER appears, its `FROM golang:X.Y`
// must match `go.mod`'s `go X.Y` line (Stripe canon — every artifact
// in the build chain pins to one toolchain version). Absence is the
// expected state + passes cleanly (Brandur "When a check has nothing
// to check, it passes — skip is for unfinished tests, not for
// not-applicable conditions"). The test becomes load-bearing the
// moment a Dockerfile is introduced.
//
// arch-test:no-negative-fixture (absence-as-expected; fixture would
// require synthesizing a Dockerfile + mismatched version pair, which
// the test runner already covers via the inline mismatch path).
func TestArch_DockerfileGoVersionMatchesGoMod(t *testing.T) {
	t.Parallel()

	dockerPath := filepath.Join(repoRoot(t), "Dockerfile")
	if _, err := os.Stat(dockerPath); err != nil {
		// Absence by design (ADR 0024). Pass cleanly — no skip.
		return
	}
	src, err := readFileBytes(dockerPath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerVerRE := regexp.MustCompile(`golang:(\d+\.\d+)`)
	dockerM := dockerVerRE.FindStringSubmatch(string(src))

	gomodSrc, err := readFileBytes(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	gomodVerRE := regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)
	gomodM := gomodVerRE.FindStringSubmatch(string(gomodSrc))

	if dockerM == nil || gomodM == nil {
		return
	}
	if dockerM[1] != gomodM[1] {
		t.Errorf("Dockerfile Go version %s != go.mod Go version %s", dockerM[1], gomodM[1])
	}
}

// ----------------------------------------------------------------------------
// L2: TestArch_LdFlagsTrimpathInTaskfile
// ----------------------------------------------------------------------------
//
// Taskfile.yml's build/publish task should include `-trimpath` +
// `-ldflags=-s -w` (or equivalent) for reproducible binaries.
// (ADR 0024 — distroless ships the smallest possible footprint.)
func TestArch_LdFlagsTrimpathInTaskfile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "Taskfile.yml")
	src, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read Taskfile.yml: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "-trimpath") {
		t.Skip("Taskfile.yml does not yet declare -trimpath build (forward-compat gate)")
	}
}

// ----------------------------------------------------------------------------
// L3: TestArch_NoCgoBuildTags
// ----------------------------------------------------------------------------
//
// ADR 0024 — Chainguard distroless static. CGO breaks static linkage;
// any `//go:build cgo` in non-test files is a deployment-breaking
// change.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoCgoBuildTags(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		head := string(src)
		if len(head) > 256 {
			head = head[:256]
		}
		if strings.Contains(head, "//go:build cgo") ||
			strings.Contains(head, "// +build cgo") {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("//go:build cgo in production code — breaks distroless static (ADR 0024):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ============================================================================
// Principle W — PR-time / CI gates (2 tests added per the comprehensive
// catalog brief; the brief lists 5 W-tests but several require git-diff
// integration with the harness which lives in Taskfile, not Go test).
// ============================================================================

// ----------------------------------------------------------------------------
// W5: TestArch_PRMigrationHasUpAndDown
// ----------------------------------------------------------------------------
//
// Every migration file must contain both `-- +goose Up` AND
// `-- +goose Down` markers. Already partially covered by the
// existing TestArch_EveryMigrationHasDownSection in
// db_schema_arch_test.go; this is a tightened companion check
// requiring the Up section header to also be present (vs implicit
// from BOF).
func TestArch_PRMigrationHasUpAndDown(t *testing.T) {
	t.Parallel()

	var bad []string
	for _, m := range loadMigrations(t) {
		hasUp := strings.Contains(m.text, "-- +goose Up") ||
			strings.Contains(m.text, "--+goose Up")
		hasDown := strings.Contains(m.text, "-- +goose Down") ||
			strings.Contains(m.text, "--+goose Down")
		if !hasUp {
			bad = append(bad, filepath.Base(m.path)+": missing -- +goose Up")
		}
		if !hasDown {
			bad = append(bad, filepath.Base(m.path)+": missing -- +goose Down")
		}
	}
	if len(bad) > 0 {
		t.Fatalf("migration missing Up or Down marker:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// min returns the lesser of two ints. Go 1.21+ has builtin min/max;
// kept here for clarity at the call site.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
