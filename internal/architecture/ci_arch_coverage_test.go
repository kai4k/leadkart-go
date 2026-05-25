// ci_arch_coverage_test.go — Fitness functions gating CI vs Taskfile
// drift across the FAANG-style reusable-workflow layout.
//
// HISTORY:
//   - Pre-Wave-9.3: cloud CI's architecture job ran a hardcoded subset
//     of arch packages. Boundary + route tests silently never ran on
//     PRs. Wave 9.3 extended CI to identity's three packages.
//   - May 2026: drift recurred. Local Taskfile had 14 paths; CI re-
//     encoded only 3. Every PR since inventory-slice-1 was merging with
//     only identity arch coverage cloud-side.
//   - June 2026: structural fix — CI workflow stopped encoding the
//     package list. The architecture job invoked `task ci:test:arch`;
//     Taskfile's ARCH_TEST_PACKAGES var became the single source of
//     truth.
//   - June 2026 (this commit): full FAANG-canon refactor — ci.yml split
//     into orchestrator + reusable workflows under .github/workflows/_*.yml.
//     Each test job is now a short `uses: ./.github/workflows/_go-test.yml`
//     block that passes `task-name:` as an input. The reusable workflow
//     invokes the task. The discipline gate moves UP one level: assert
//     each test job in ci.yml supplies the canonical task-name.
//
// Ford / Parsons / Kua "Building Evolutionary Architectures" canon —
// fitness functions encode invariants as executable tests. The new
// invariant is "ci.yml's <job> block sets `task-name: <expected>` when
// calling _go-test.yml." Drift = the task-name input doesn't match,
// caught at PR time.

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_CIUnitJobInvokesTaskRunner asserts the unit job in ci.yml
// calls the reusable _go-test.yml workflow with task-name: ci:test.
//
// arch-test:no-negative-fixture — the assertion target IS the shipping
// .github/workflows/ci.yml. A synthetic fixture would defeat the
// "guards the actual CI" guarantee. Ford / Parsons / Kua ch.4 — fitness
// functions may opt out of negative fixtures when the assertion target
// is the production artifact itself.
func TestArch_CIUnitJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()
	assertJobInvokesTask(t, "unit", "ci:test", "ci:test\\b")
}

// TestArch_CIArchJobInvokesTaskRunner asserts the architecture job in
// ci.yml calls _go-test.yml with task-name: ci:test:arch.
//
// arch-test:no-negative-fixture — same rationale as the sibling tests.
func TestArch_CIArchJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()
	assertJobInvokesTask(t, "architecture", "ci:test:arch", "ci:test:arch\\b")
}

// TestArch_CIIntegrationJobInvokesTaskRunner asserts the integration
// job in ci.yml calls _go-test.yml with task-name: ci:test:int:shard
// (matrix-sharded variant). The job must ALSO declare a strategy.matrix
// for parallel sharding — serializing 7 modules onto one runner would
// re-introduce the 5+ min wall-time the sharding was added to kill.
//
// arch-test:no-negative-fixture — same rationale as the sibling tests.
func TestArch_CIIntegrationJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()
	jobBlock := assertJobInvokesTask(t, "integration", "ci:test:int:shard", "ci:test:int:shard\\b")
	if jobBlock == "" {
		return
	}
	if !regexp.MustCompile(`(?m)^\s+strategy:\s*$`).MatchString(jobBlock) ||
		!regexp.MustCompile(`(?m)^\s+matrix:\s*$`).MatchString(jobBlock) {
		t.Errorf("ci.yml integration job MUST declare strategy.matrix for parallel module sharding (collapsing to a single runner regresses to 5+min wall-time per ADR-implicit fail-fast canon).")
	}
}

// TestArch_CIOpenAPIJobInvokesReusable asserts the openapi-lint job in
// ci.yml dispatches to _openapi.yml (which pins Spectral via Taskfile
// SPECTRAL_VERSION). Forbid inline npx in the orchestrator itself.
//
// arch-test:no-negative-fixture — same rationale as the sibling tests.
func TestArch_CIOpenAPIJobInvokesReusable(t *testing.T) {
	t.Parallel()

	body := mustReadFile(t, filepath.Join(repoRootDir(t), ".github", "workflows", "ci.yml"))
	apiJob := extractCIJobBlock(t, body, "openapi-lint")
	if apiJob == "" {
		t.Skip("ci.yml: no `openapi-lint:` job present")
	}

	if !regexp.MustCompile(`uses:\s*\./.github/workflows/_openapi\.yml`).MatchString(apiJob) {
		t.Errorf("ci.yml openapi-lint job MUST `uses: ./.github/workflows/_openapi.yml` (Spectral version is pinned via Taskfile SPECTRAL_VERSION).")
	}
	if regexp.MustCompile(`(?m)npx[^\n]*@latest\b`).MatchString(apiJob) {
		t.Errorf("ci.yml openapi-lint job MUST NOT use `npx ...@latest` — supply-chain anti-pattern (govulncheck canon).")
	}
}

// TestArch_CIReusableTestWorkflowInvokesTask asserts the reusable
// _go-test.yml workflow actually invokes `task <name>` (the discipline
// gate at the bottom of the call stack). Ensures the orchestrator
// can't sneak inline `go test` past the gate by routing through the
// reusable workflow.
//
// arch-test:no-negative-fixture — same rationale.
func TestArch_CIReusableTestWorkflowInvokesTask(t *testing.T) {
	t.Parallel()

	body := mustReadFile(t, filepath.Join(repoRootDir(t), ".github", "workflows", "_go-test.yml"))

	if !regexp.MustCompile(`run:\s*task\s+\$\{\{\s*inputs\.task-name\s*\}\}`).MatchString(body) {
		t.Errorf("_go-test.yml MUST invoke `task ${{ inputs.task-name }}` (orchestrator passes task names; reusable workflow runs them).")
	}
	forbiddenLineRE := regexp.MustCompile(`(?m)^\s*(go\s+test\b|gotestsum\b|go\s+tool\s+gotestsum\b)`)
	if matches := forbiddenLineRE.FindAllString(body, -1); len(matches) > 0 {
		t.Errorf("_go-test.yml MUST NOT inline `go test` / `gotestsum` — Taskfile owns the command shape. Found %d:", len(matches))
		for _, m := range matches {
			t.Errorf("  forbidden: %q", strings.TrimSpace(m))
		}
	}
}

// assertJobInvokesTask asserts that the named job in ci.yml calls a
// reusable workflow under ./.github/workflows/_*.yml AND supplies
// `task-name: <expected>` (or a matching regex) as an input. Returns
// the job block string (for the caller to do additional checks) or "".
func assertJobInvokesTask(t *testing.T, jobName, expectedTask, taskRegex string) string {
	t.Helper()

	body := mustReadFile(t, filepath.Join(repoRootDir(t), ".github", "workflows", "ci.yml"))
	jobBlock := extractCIJobBlock(t, body, jobName)
	if jobBlock == "" {
		t.Fatalf("ci.yml: could not locate `%s:` job block", jobName)
	}

	if !regexp.MustCompile(`uses:\s*\./.github/workflows/_[a-z0-9-]+\.yml`).MatchString(jobBlock) {
		t.Errorf("ci.yml `%s` job MUST dispatch to a reusable workflow under .github/workflows/_*.yml.", jobName)
		return jobBlock
	}

	taskInputRE := regexp.MustCompile(`task-name:\s*` + taskRegex)
	if !taskInputRE.MatchString(jobBlock) {
		t.Errorf("ci.yml `%s` job MUST supply `task-name: %s` to the reusable workflow.", jobName, expectedTask)
	}

	// Forbid inline test invocations in the orchestrator — even though
	// they wouldn't run (orchestrator dispatches to reusable workflows),
	// their presence indicates someone tried to bypass the discipline.
	forbiddenLineRE := regexp.MustCompile(`(?m)^\s*(go\s+test\b|gotestsum\b|go\s+tool\s+gotestsum\b)`)
	if matches := forbiddenLineRE.FindAllString(jobBlock, -1); len(matches) > 0 {
		t.Errorf("ci.yml `%s` job MUST NOT inline `go test` / `gotestsum` — dispatch to reusable workflow + task. Found %d:", jobName, len(matches))
		for _, m := range matches {
			t.Errorf("  forbidden: %q", strings.TrimSpace(m))
		}
	}
	return jobBlock
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
