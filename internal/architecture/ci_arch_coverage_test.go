// ci_arch_coverage_test.go — Fitness functions gating CI vs Taskfile drift.
//
// FAANG reusable-workflow layout: ci.yml dispatches to _go-test.yml with
// `task-name: <task>`. Each job must supply the canonical task-name so
// Taskfile owns the command shape (Ford/Parsons/Kua: fitness function).

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_CIUnitJobInvokesTaskRunner asserts the unit job in ci.yml calls
// _go-test.yml with task-name: ci:test.
//
// arch-test:no-negative-fixture — assertion target is the shipping ci.yml.
func TestArch_CIUnitJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()
	assertJobInvokesTask(t, "unit", "ci:test", "ci:test\\b")
}

// TestArch_CIArchJobInvokesTaskRunner asserts the architecture job in
// ci.yml calls _go-test.yml with task-name: ci:test:arch.
//
// arch-test:no-negative-fixture — assertion target is the shipping ci.yml.
func TestArch_CIArchJobInvokesTaskRunner(t *testing.T) {
	t.Parallel()
	assertJobInvokesTask(t, "architecture", "ci:test:arch", "ci:test:arch\\b")
}

// TestArch_CIIntegrationJobInvokesTaskRunner asserts the integration job calls
// _go-test.yml with task-name: ci:test:int:shard and declares strategy.matrix.
//
// arch-test:no-negative-fixture — assertion target is the shipping ci.yml.
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

// TestArch_CIOpenAPIJobInvokesReusable asserts the openapi-lint job dispatches
// to _openapi.yml with no inline npx.
//
// arch-test:no-negative-fixture — assertion target is the shipping ci.yml.
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

// TestArch_CIReusableTestWorkflowInvokesTask asserts _go-test.yml invokes
// `task ${{ inputs.task-name }}` with no inline go test / gotestsum.
//
// arch-test:no-negative-fixture — assertion target is the shipping _go-test.yml.
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

// assertJobInvokesTask asserts the named job dispatches to a reusable _*.yml
// workflow with task-name: expected. Returns the job block.
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

	forbiddenLineRE := regexp.MustCompile(`(?m)^\s*(go\s+test\b|gotestsum\b|go\s+tool\s+gotestsum\b)`)
	if matches := forbiddenLineRE.FindAllString(jobBlock, -1); len(matches) > 0 {
		t.Errorf("ci.yml `%s` job MUST NOT inline `go test` / `gotestsum` — dispatch to reusable workflow + task. Found %d:", jobName, len(matches))
		for _, m := range matches {
			t.Errorf("  forbidden: %q", strings.TrimSpace(m))
		}
	}
	return jobBlock
}

// extractCIJobBlock returns the lines for a named top-level CI job block.
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

// mustReadFile reads a file, fataling the test on error.
func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // arch-test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// repoRootDir returns the repo root by walking up until go.mod is found.
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
