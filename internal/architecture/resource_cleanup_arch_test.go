// resource_cleanup_arch_test.go — Principle G: Resource cleanup discipline.
//
// File handles, DB connections, HTTP response bodies, pgx rows, txs —
// every Acquire/Open MUST pair with a Release/Close. Long-running
// services accumulate leaks silently until OOM or pool exhaustion.
//
// Cited canon:
//   - Cheney "Practical Go" (defer patterns)
//   - Brandur "go-database/sql Done Right" + pgx README
//   - Go stdlib docs — net/http Response.Body
//   - Brad Fitzpatrick — "errgroup" canonical talks

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// G1: TestArch_DeferCloseAfterOpen
// ----------------------------------------------------------------------------
//
// File / connection / response-body opens must be followed within
// 3 lines by `defer X.Close()`. Generic pattern check; covers
// os.Open / os.Create / http.Get response bodies / file.OpenFile.
//
// Allow-list: tests that explicitly close-and-check err (`if err :=
// f.Close(); err != nil`) on the same line are fine.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_DeferCloseAfterOpen(t *testing.T) {
	t.Parallel()

	openRE := regexp.MustCompile(`(\w+),\s*(?:err\s*:?=|_\s*:?=)\s*os\.(?:Open|Create|OpenFile)\(`)
	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			m := openRE.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			handle := m[1]
			deferRE := regexp.MustCompile(`defer\s+` + regexp.QuoteMeta(handle) + `\.Close\(\)`)
			found := false
			for j := i; j < len(lines) && j < i+8; j++ {
				if deferRE.MatchString(lines[j]) {
					found = true
					break
				}
			}
			if !found {
				bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("os.Open/Create without defer Close() within 8 lines (handle leak):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// G2: TestArch_RowsCloseErrorChecked
// ----------------------------------------------------------------------------
//
// `rows.Close()` is the second-most-forgotten cleanup site (after
// http.Response.Body). pgx rows returned from Query MUST be closed;
// the `defer rows.Close()` idiom is mandatory.
//
// Predicate: every `Query(`-returning assignment (pgx pattern
// `rows, err := <something>.Query(`) must be followed within 5
// lines by `defer rows.Close()` (or the assignment is to `_`).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RowsCloseErrorChecked(t *testing.T) {
	t.Parallel()

	queryRE := regexp.MustCompile(`(\w+),\s*err\s*:?=\s*\w+\.Query\(`)
	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return // sqlc-generated; uses its own DBTX shape
		}
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			m := queryRE.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			handle := m[1]
			if handle == "_" {
				continue
			}
			deferRE := regexp.MustCompile(`defer\s+` + regexp.QuoteMeta(handle) + `\.Close\(\)`)
			found := false
			for j := i; j < len(lines) && j < i+6; j++ {
				if deferRE.MatchString(lines[j]) {
					found = true
					break
				}
			}
			if !found {
				bad = append(bad, slash+":"+itoa(i+1))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf(".Query(...) without defer rows.Close() within 6 lines (connection leak):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// G3: TestArch_TxRollbackInErrorPath
// ----------------------------------------------------------------------------
//
// Manual `tx.Begin()` outside the Transactor wrapper MUST have
// `defer tx.Rollback()` as the immediate follow-up. Rollback after
// Commit is a no-op; rollback without Commit cleans up. This is the
// canonical Go idiom (database/sql + pgx README).
//
// Predicate: lines matching `<x>, err := <y>.Begin*(...)` followed
// within 5 lines by `defer <x>.Rollback(ctx)`.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_TxRollbackInErrorPath(t *testing.T) {
	t.Parallel()

	beginRE := regexp.MustCompile(`(\w+),\s*err\s*:?=\s*\w+\.Begin(?:Tx)?\(`)
	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		// The Transactor substrate is allowed to manage its own
		// rollback explicitly (commit-or-rollback branch).
		if strings.Contains(slash, "/common/pg/") {
			return
		}
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			m := beginRE.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			tx := m[1]
			deferRE := regexp.MustCompile(`defer\s+` + regexp.QuoteMeta(tx) + `\.Rollback\(`)
			found := false
			for j := i; j < len(lines) && j < i+6; j++ {
				if deferRE.MatchString(lines[j]) {
					found = true
					break
				}
			}
			if !found {
				bad = append(bad, slash+":"+itoa(i+1))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("manual tx.Begin() without defer tx.Rollback() within 6 lines:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// G4: TestArch_NoLeakedFileHandles
// ----------------------------------------------------------------------------
//
// `os.OpenFile` (with explicit flags) is the canonical file-open;
// stricter cousin of G1. Predicate: every os.OpenFile-returned
// handle has a defer Close.
//
// Already partially covered by G1 (which detects Open + Create +
// OpenFile under the same regex). G4 narrows to OpenFile with
// non-default flags (write-mode) since those tend to hold the lock
// longer.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoLeakedFileHandles(t *testing.T) {
	t.Parallel()

	writeOpenRE := regexp.MustCompile(`(\w+),\s*err\s*:?=\s*os\.OpenFile\(.*O_(?:WRONLY|RDWR|CREATE|APPEND)`)
	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			m := writeOpenRE.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			handle := m[1]
			deferRE := regexp.MustCompile(`defer\s+` + regexp.QuoteMeta(handle) + `\.Close\(\)`)
			found := false
			for j := i; j < len(lines) && j < i+8; j++ {
				if deferRE.MatchString(lines[j]) {
					found = true
					break
				}
			}
			if !found {
				bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("os.OpenFile (write mode) without defer Close — file lock leak:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// G5: TestArch_NoSyncOnceLeaksGoroutines
// ----------------------------------------------------------------------------
//
// `sync.Once.Do(fn)` bodies that spawn goroutines without an explicit
// shutdown path leak forever (the Once never re-runs). Predicate:
// any `\.Do\(func\b` body must NOT contain `go func` or `go <call>`
// unless explicitly marked.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoSyncOnceLeaksGoroutines(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	doceRE := regexp.MustCompile(`\.Do\(\s*func\s*\(\s*\)`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		idx := doceRE.FindAllStringIndex(body, -1)
		for _, loc := range idx {
			// Slice ~400 chars forward as the once-body window.
			end := loc[1] + 400
			if end > len(body) {
				end = len(body)
			}
			window := body[loc[1]:end]
			if strings.Contains(window, "go func") || strings.Contains(window, "\tgo ") {
				if !strings.Contains(window, "arch-test:once-spawns-goroutine") {
					bad = append(bad, pathToSlash(path)+":"+itoa(lineNumberAt(body, loc[0])))
				}
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("sync.Once.Do body spawns goroutine (forever-running; opt out via arch-test:once-spawns-goroutine):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// (compile-anchor for filepath import)
var _ = filepath.Join
