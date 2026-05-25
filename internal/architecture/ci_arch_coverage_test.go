// ci_arch_coverage_test.go — Fitness function gating CI vs local
// architecture-test coverage drift.
//
// HISTORY:
//   - Pre-Wave-9.3: cloud CI's architecture job ran a hardcoded subset
//     of arch packages (`integrationevents/...` only) while local
//     `task test:arch` covered the full identity surface. Boundary +
//     route tests silently never ran on PRs. Wave 9.3 extended CI to
//     identity's three packages.
//   - May 2026: drift recurred — local Taskfile grew to 14 paths
//     (inventory + platform + crm + the cross-cutting
//     internal/architecture/ fitness-function suite, 98+ tests) while
//     CI still listed only the three identity paths. Every PR since
//     inventory-slice-1 was merging with only identity arch coverage
//     cloud-side.
//
// The bug recurs because the source-of-truth lives in Taskfile.yml but
// .github/workflows/ci.yml re-encodes the list. This test makes the
// invariant first-class: parse both files, extract the package lists,
// fail if they disagree.
//
// Ford / Parsons / Kua "Building Evolutionary Architectures" canon —
// fitness functions encode invariants as executable tests so drift
// becomes a PR-time failure, not a "we noticed three months later"
// incident.
//
// Scope: production discipline — applies to the CI pipeline + Taskfile
// itself, both of which are project-level shipping artifacts.

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestArch_CIArchJobCoversAllArchPackages asserts that the package list
// the CI architecture job runs (in .github/workflows/ci.yml) is an
// EXACT superset of the package list local `task test:arch` runs
// (in Taskfile.yml). Drift = CI silently skips arch packages = the
// fitness functions don't gate the PR.
//
// arch-test:no-negative-fixture — this test parses the REAL ci.yml +
// Taskfile.yml at the project root (not path-injectable fixtures); a
// synthetic fixture would defeat the "asserts against the actual
// shipping CI" guarantee. The test IS the fixture: tampering with
// either file in a way that creates drift is the negative case and
// fails the test immediately. Ford / Parsons / Kua ch. 4 — fitness
// functions may opt out of negative fixtures when the assertion target
// is the production artifact itself.
func TestArch_CIArchJobCoversAllArchPackages(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootDir(t)

	taskfilePkgs := extractTaskfileArchPackages(t, filepath.Join(repoRoot, "Taskfile.yml"))
	if len(taskfilePkgs) == 0 {
		t.Fatal("Taskfile.yml: no arch packages extracted — parser regression?")
	}

	ciPkgs := extractCIArchPackages(t, filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))
	if len(ciPkgs) == 0 {
		t.Fatal(".github/workflows/ci.yml: no arch packages extracted — parser regression?")
	}

	ciSet := make(map[string]struct{}, len(ciPkgs))
	for _, p := range ciPkgs {
		ciSet[p] = struct{}{}
	}

	var missingInCI []string
	for _, p := range taskfilePkgs {
		if _, ok := ciSet[p]; !ok {
			missingInCI = append(missingInCI, p)
		}
	}

	taskfileSet := make(map[string]struct{}, len(taskfilePkgs))
	for _, p := range taskfilePkgs {
		taskfileSet[p] = struct{}{}
	}
	var extraInCI []string
	for _, p := range ciPkgs {
		if _, ok := taskfileSet[p]; !ok {
			extraInCI = append(extraInCI, p)
		}
	}

	sort.Strings(missingInCI)
	sort.Strings(extraInCI)

	if len(missingInCI) == 0 && len(extraInCI) == 0 {
		return
	}

	t.Logf("CI ↔ Taskfile arch-package drift detected.")
	t.Logf("Local `task test:arch` runs %d packages; CI architecture job runs %d packages.",
		len(taskfilePkgs), len(ciPkgs))
	if len(missingInCI) > 0 {
		t.Errorf("%d package(s) listed in Taskfile.yml `test:arch` but MISSING from .github/workflows/ci.yml architecture job:", len(missingInCI))
		for _, p := range missingInCI {
			t.Errorf("  TASKFILE→ %s — add this path to the gotestsum invocation in ci.yml", p)
		}
	}
	if len(extraInCI) > 0 {
		t.Errorf("%d package(s) listed in ci.yml architecture job but MISSING from Taskfile.yml `test:arch`:", len(extraInCI))
		for _, p := range extraInCI {
			t.Errorf("  CI→ %s — add this path to the `test:arch` cmd in Taskfile.yml", p)
		}
	}
}

// TestArch_CIArchJobUsesCanonicalRunRegex asserts that the gotestsum
// `-run` regex in ci.yml matches local `task test:arch` shape — i.e.
// `^Test(Arch|Meta)_` (covers both TestArch_* fitness functions and
// TestMeta_* scope-marker helpers). Earlier drift had CI on `^TestArch_`
// while local also ran TestMeta_*.
//
// arch-test:no-negative-fixture — same rationale as the sibling test
// above: assertion targets the actual ci.yml shipping artifact, not a
// fixture file. The negative case is "edit ci.yml to use a different
// regex" — caught immediately on next run.
func TestArch_CIArchJobUsesCanonicalRunRegex(t *testing.T) {
	t.Parallel()

	canon := `^Test(Arch|Meta)_`

	repoRoot := repoRootDir(t)
	body := mustReadFile(t, filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))

	// Look for the gotestsum -run argument anywhere in the architecture
	// job. Pattern: `-run "<regex>"` (double-quoted) inside the gotestsum
	// command of the `architecture:` job.
	// Extract every `-run "..."` occurrence; require at least one match
	// = canon, no mismatched ones.
	runRE := regexp.MustCompile(`-run\s+"([^"]+)"`)
	matches := runRE.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("ci.yml: could not find any `-run \"...\"` argument in the architecture job — workflow shape changed?")
	}

	found := false
	var bad []string
	for _, m := range matches {
		if m[1] == canon {
			found = true
			continue
		}
		bad = append(bad, m[1])
	}
	if !found {
		t.Errorf("ci.yml architecture job: expected `-run \"%s\"` to gate both TestArch_* + TestMeta_*; got %v", canon, bad)
	}
}

// extractTaskfileArchPackages parses Taskfile.yml and pulls the package
// paths from the `test:arch:` task's `cmds:` block. The cmd line is one
// long `go test ... ./pkg1/... ./pkg2/... ...` invocation; we extract
// every `./internal/.../...` token.
func extractTaskfileArchPackages(t *testing.T, path string) []string {
	t.Helper()

	body := mustReadFile(t, path)

	// Locate the `test:arch:` block + its `cmds:` line. The Taskfile
	// uses one cmd line that includes the full package list.
	taskBlock := extractYAMLBlock(body, "test:arch:")
	if taskBlock == "" {
		t.Fatalf("Taskfile.yml: could not locate `test:arch:` block")
	}

	pkgRE := regexp.MustCompile(`(\./internal/[\w/.-]+/\.\.\.)`)
	matches := pkgRE.FindAllStringSubmatch(taskBlock, -1)

	seen := map[string]struct{}{}
	var out []string
	for _, m := range matches {
		if _, dup := seen[m[1]]; dup {
			continue
		}
		seen[m[1]] = struct{}{}
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// extractCIArchPackages parses ci.yml and pulls the package paths from
// the `architecture:` job's `Run architecture tests` step. The step
// uses a multi-line shell command with one package path per
// backslash-continued line.
func extractCIArchPackages(t *testing.T, path string) []string {
	t.Helper()

	body := mustReadFile(t, path)

	// Locate the `architecture:` job block. It runs until the next
	// top-level job (de-indented two spaces back to column 2).
	idx := strings.Index(body, "\n  architecture:")
	if idx < 0 {
		t.Fatalf("ci.yml: could not locate `architecture:` job block")
	}
	rest := body[idx+1:]
	// End of block = next line that starts with exactly two spaces +
	// a word (i.e. another top-level job). Or EOF.
	endRE := regexp.MustCompile(`(?m)^  [a-z][a-zA-Z0-9_-]+:\s*$`)
	loc := endRE.FindStringIndex(rest[len("  architecture:"):])
	jobBlock := rest
	if loc != nil {
		jobBlock = rest[:len("  architecture:")+loc[0]]
	}

	pkgRE := regexp.MustCompile(`(\./internal/[\w/.-]+/\.\.\.)`)
	matches := pkgRE.FindAllStringSubmatch(jobBlock, -1)

	seen := map[string]struct{}{}
	var out []string
	for _, m := range matches {
		if _, dup := seen[m[1]]; dup {
			continue
		}
		seen[m[1]] = struct{}{}
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// extractYAMLBlock returns the lines under `key:` (inclusive of the
// key line) until the next sibling key at the same indent. Naive
// indent-based parser — sufficient for the Taskfile's flat shape; not
// for general YAML.
func extractYAMLBlock(body, key string) string {
	lines := strings.Split(body, "\n")
	startIdx := -1
	keyIndent := -1
	for i, ln := range lines {
		trimmed := strings.TrimLeft(ln, " ")
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		// require the rest of the line to be empty (key with no value)
		// to avoid matching a comment that mentions the key.
		if strings.TrimSpace(ln) != key {
			continue
		}
		startIdx = i
		keyIndent = len(ln) - len(trimmed)
		break
	}
	if startIdx < 0 {
		return ""
	}
	end := len(lines)
	for i := startIdx + 1; i < len(lines); i++ {
		ln := lines[i]
		if strings.TrimSpace(ln) == "" {
			continue
		}
		trimmed := strings.TrimLeft(ln, " ")
		indent := len(ln) - len(trimmed)
		if indent <= keyIndent {
			end = i
			break
		}
	}
	return strings.Join(lines[startIdx:end], "\n")
}

// mustReadFile returns the file body or fails the test.
func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // arch-test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// repoRootDir returns the absolute path to the repo root by walking up
// from the test file's directory until it finds the go.mod.
func repoRootDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoRootDir: walked to filesystem root without finding go.mod (started at %s)", wd)
		}
		dir = parent
	}
}
