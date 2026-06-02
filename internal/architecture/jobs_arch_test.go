// jobs_arch_test.go — Principle I: River background-job discipline.
//
// ADR 0010: river is the only job library. Jobs must declare Kind(), Timeout,
// and idempotent semantics.

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_RiverJobsImplementInterface asserts river job files expose Kind() string.
// Vacuously true if no jobs/ dir exists yet.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RiverJobsImplementInterface(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string
	any := false

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "jobs")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			any = true
			body := stripGoComments(string(src))
			hasKind := strings.Contains(body, "Kind() string") ||
				strings.Contains(body, "Kind()    string")
			hasRiver := strings.Contains(body, "river.")
			if !hasKind && hasRiver {
				bad = append(bad, pathToSlash(path))
			}
		})
	}
	_ = any

	if len(bad) > 0 {
		t.Fatalf("river-jobs file doesn't expose Kind() string — required by river.Worker:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_RiverJobsHaveTimeout asserts river workers declare an explicit Timeout.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RiverJobsHaveTimeout(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "jobs")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			if !strings.Contains(body, "river.") {
				return
			}
			if !strings.Contains(body, "Timeout(") &&
				!strings.Contains(body, "Timeout:") {
				bad = append(bad, pathToSlash(path))
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("river worker missing explicit Timeout — workers can hang forever:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// `// river:idempotent <reason>` documentation comment on the
// documentation (retry semantics require idempotent handlers).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RiverJobsIdempotent(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "jobs")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := string(src)
			if !strings.Contains(body, "river.") {
				return
			}
			if !strings.Contains(body, "// river:idempotent") &&
				!strings.Contains(body, "// idempotent:") {
				bad = append(bad, pathToSlash(path))
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("river worker missing // river:idempotent doc comment (retry assumption):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_RiverQueuesScopedPerModule asserts queue names match ^[a-z]+_[a-z_]+$
// (module_purpose shape for per-module scaling).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RiverQueuesScopedPerModule(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	queueRE := regexp.MustCompile(`(?:Queue|QueueName)\s*:\s*"([^"]+)"`)
	moduleRE := regexp.MustCompile(`^[a-z]+_[a-z_]+$`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "jobs")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := string(src)
			for _, m := range queueRE.FindAllStringSubmatch(body, -1) {
				q := m[1]
				if !moduleRE.MatchString(q) {
					bad = append(bad, pathToSlash(path)+": "+q)
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("river queue name doesn't match ^[a-z]+_[a-z_]+\\$ (module_purpose):\n  %s",
			strings.Join(bad, "\n  "))
	}
}
