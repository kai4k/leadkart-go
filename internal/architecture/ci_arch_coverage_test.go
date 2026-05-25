// ci_arch_coverage_test.go — Fitness function gating CI vs Taskfile
// drift for the architecture-test job.
//
// HISTORY:
//   - Pre-Wave-9.3: cloud CI's architecture job ran a hardcoded subset
//     of arch packages (`integrationevents/...` only). Boundary + route
//     tests silently never ran on PRs. Wave 9.3 extended CI to
//     identity's three packages — duplication remained.
//   - May 2026: drift recurred. Local Taskfile had grown to 14 paths;
//     CI re-encoded only 3. Every PR since inventory-slice-1 was
//     merging with only identity arch coverage cloud-side.
//   - June 2026 (this commit): structural fix — CI no longer encodes
//     the package list at all. The architecture job invokes
//     `task ci:test:arch`; the Taskfile's `ARCH_TEST_PACKAGES` var is
//     the SINGLE source of truth. The test below pins that invariant.
//
// Ford / Parsons / Kua "Building Evolutionary Architectures" canon —
// fitness functions encode invariants as executable tests. The new
// invariant is "CI invokes the task runner; never reimplements." This
// is structurally stronger than the previous "two encoded lists must
// stay in sync" — there's now only one list, so drift is impossible.
//
// Scope: production discipline — applies to the CI pipeline (shipping
// artifact) + Taskfile (the single source of truth for command shape).

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_CIArchJobInvokesTaskRunner asserts that the CI architecture
// job calls the Taskfile (via `task ci:test:arch`) rather than
// re-encoding `go test` / `gotestsum` invocations + a hand-maintained
// package list. The Taskfile is the canonical source of truth (Brandur
// Leach + golangci-lint canon — build commands live in the build script
// or task runner, CI invokes them by name).
//
// arch-test:no-negative-fixture — the assertion target IS the shipping
// .github/workflows/ci.yml. A synthetic fixture would defeat the
// "guards the actual CI" guarantee. Tampering with ci.yml in a way
// that re-introduces an inline `gotestsum -- go test ...` is the
// negative case + fails the test immediately. Ford / Parsons / Kua
// ch.4 — fitness functions may opt out of negative fixtures when the
// assertion target is the production artifact itself.
func TestArch_CIArchJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()

	body := mustReadFile(t, filepath.Join(repoRootDir(t), ".github", "workflows", "ci.yml"))

	archJob := extractCIJobBlock(t, body, "architecture")
	if archJob == "" {
		t.Fatal("ci.yml: could not locate `architecture:` job block")
	}

	// Canon: the architecture job's main step runs `task ci:test:arch`.
	// Allow either `run: task ci:test:arch` (single-line) or it appearing
	// inside a multi-line `run: |` block.
	taskInvocationRE := regexp.MustCompile(`(?m)^\s*-?\s*(run:\s*|.*\|\s*$)?\s*task\s+ci:test:arch\b`)
	if !taskInvocationRE.MatchString(archJob) {
		t.Errorf("ci.yml architecture job MUST invoke `task ci:test:arch` (not hand-rolled gotestsum/go-test).")
		t.Errorf("Canon: Taskfile.yml owns the package list + flags via ARCH_TEST_PACKAGES var + ci:test:arch task; CI just invokes the task.")
	}

	// Reverse direction: forbid `go test` / direct `gotestsum` lines in
	// the architecture job — drift would mean re-encoding the package
	// list cloud-side, which is the exact bug this gate exists to
	// prevent.
	forbiddenLineRE := regexp.MustCompile(`(?m)^\s*(go\s+test\b|gotestsum\b|go\s+tool\s+gotestsum\b)`)
	if matches := forbiddenLineRE.FindAllString(archJob, -1); len(matches) > 0 {
		t.Errorf("ci.yml architecture job MUST NOT inline `go test` / `gotestsum` invocations — found %d:", len(matches))
		for _, m := range matches {
			t.Errorf("  forbidden: %q — replace with `task ci:test:arch`", strings.TrimSpace(m))
		}
	}
}

// TestArch_CIUnitJobInvokesTaskRunner mirrors the architecture-job rule
// for the unit-test job. Same canon — Taskfile owns the command shape
// (gotestsum wrapper, -race, -shuffle, -coverprofile, -skip regex); CI
// invokes by name. The previous shape had 6 inline flags duplicated
// between Taskfile + ci.yml.
//
// arch-test:no-negative-fixture — same rationale as the sibling test
// above.
func TestArch_CIUnitJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()

	body := mustReadFile(t, filepath.Join(repoRootDir(t), ".github", "workflows", "ci.yml"))
	unitJob := extractCIJobBlock(t, body, "unit")
	if unitJob == "" {
		t.Fatal("ci.yml: could not locate `unit:` job block")
	}

	taskInvocationRE := regexp.MustCompile(`(?m)task\s+ci:test\b`)
	if !taskInvocationRE.MatchString(unitJob) {
		t.Errorf("ci.yml unit job MUST invoke `task ci:test` (not hand-rolled gotestsum/go-test).")
	}
}

// TestArch_CIIntegrationJobInvokesTaskRunner — same canon for the
// integration job.
//
// arch-test:no-negative-fixture — same rationale.
func TestArch_CIIntegrationJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()

	body := mustReadFile(t, filepath.Join(repoRootDir(t), ".github", "workflows", "ci.yml"))
	intJob := extractCIJobBlock(t, body, "integration")
	if intJob == "" {
		t.Fatal("ci.yml: could not locate `integration:` job block")
	}

	taskInvocationRE := regexp.MustCompile(`(?m)task\s+ci:test:int\b`)
	if !taskInvocationRE.MatchString(intJob) {
		t.Errorf("ci.yml integration job MUST invoke `task ci:test:int` (not hand-rolled gotestsum/go-test).")
	}
}

// TestArch_CIOpenAPIJobInvokesTaskRunner — same canon for the OpenAPI
// lint job. Taskfile pins Spectral version via SPECTRAL_VERSION var;
// CI MUST NOT re-encode the npx command (a CI-side `@latest` would
// defeat the pinning + re-introduce the same supply-chain anti-pattern
// govulncheck was deliberately moved off of).
//
// arch-test:no-negative-fixture — same rationale.
func TestArch_CIOpenAPIJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()

	body := mustReadFile(t, filepath.Join(repoRootDir(t), ".github", "workflows", "ci.yml"))
	apiJob := extractCIJobBlock(t, body, "openapi-lint")
	if apiJob == "" {
		// openapi-lint job may not exist in every revision — skip gracefully.
		t.Skip("ci.yml: no `openapi-lint:` job present")
	}

	taskInvocationRE := regexp.MustCompile(`(?m)task\s+ci:openapi\b`)
	if !taskInvocationRE.MatchString(apiJob) {
		t.Errorf("ci.yml openapi-lint job MUST invoke `task ci:openapi` (Spectral version is pinned via SPECTRAL_VERSION in Taskfile.yml).")
	}

	forbiddenLineRE := regexp.MustCompile(`(?m)npx[^\n]*@latest\b`)
	if forbiddenLineRE.MatchString(apiJob) {
		t.Errorf("ci.yml openapi-lint job MUST NOT use `npx ...@latest` — supply-chain anti-pattern (govulncheck canon). Use `task ci:openapi` instead.")
	}
}

// extractCIJobBlock returns the lines belonging to a single top-level
// job in a GitHub Actions workflow file. Job blocks start at column 2
// (`  <name>:`) and end at the next sibling job at the same indent
// (or EOF).
func extractCIJobBlock(t *testing.T, body, name string) string {
	t.Helper()
	header := "\n  " + name + ":"
	idx := strings.Index(body, header)
	if idx < 0 {
		return ""
	}
	rest := body[idx+1:]
	endRE := regexp.MustCompile(`(?m)^  [a-z][a-zA-Z0-9_-]+:\s*$`)
	loc := endRE.FindStringIndex(rest[len("  "+name+":"):])
	if loc == nil {
		return rest
	}
	return rest[:len("  "+name+":")+loc[0]]
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
