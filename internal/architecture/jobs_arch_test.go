// jobs_arch_test.go — Principle I: River background-job discipline.
//
// ADR 0010: river is the only background-job library. River jobs
// implement a typed Worker interface; the canonical shape requires
// Kind(), an explicit Timeout, and idempotent semantics.
//
// Cited canon:
//   - river docs — Worker interface + Idempotent annotation
//   - Brandur Leach — river README + "Background jobs at scale"

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// I1: TestArch_RiverJobsImplementInterface
// ----------------------------------------------------------------------------
//
// Types in `<module>/jobs/` should have a `Kind() string` method.
// (River uses Kind to route + log + dedup.)
//
// Vacuously true when no jobs/ dir exists yet — the test is the
// institutional lever for when CRM/Inventory start using river.
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
			// Pragmatic: file must reference both `Kind() string` and
			// `river.Worker` (or river.Args / river.JobArgs).
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

// ----------------------------------------------------------------------------
// I2: TestArch_RiverJobsHaveTimeout
// ----------------------------------------------------------------------------
//
// Every River worker MUST declare an explicit Timeout. Defaulting
// to "wait forever" is a known cause of stuck workers.
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

// ----------------------------------------------------------------------------
// I3: TestArch_RiverJobsIdempotent
// ----------------------------------------------------------------------------
//
// River retries on failure → workers MUST be idempotent OR carry
// `// river:idempotent <reason>` documentation comment on the
// Worker type.
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

// ----------------------------------------------------------------------------
// I4: TestArch_RiverQueuesScopedPerModule
// ----------------------------------------------------------------------------
//
// Queue names follow `<module>_<purpose>`. Shared queue names mean
// CRM's slow job blocks inventory's fast job. Per-module queues
// give per-team scaling.
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
