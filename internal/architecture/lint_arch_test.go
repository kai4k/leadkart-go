// lint_arch_test.go — Principle P: Linting / static analysis discipline.
//
// The project runs golangci-lint v2 + gosec + revive + bodyclose in
// `task lint`. These tests guarantee the lint configuration itself
// stays canonical (drift in .golangci.yml is the silent
// "we stopped catching X" failure mode), every `//nolint:` directive
// has a documented reason, and TODOs carry ticket references.
//
// Cited canon:
//   - golangci-lint v2 schema docs
//   - Cheney "Practical Go" §6 (nolint with reason)
//   - Linus Torvalds — TODO comments need ticket numbers (kernel canon)

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// P1: TestArch_GolangciLintConfigCanonical
// ----------------------------------------------------------------------------
//
// .golangci.yml must enable the canonical linter set: errcheck,
// ineffassign, gosec, staticcheck, revive, govet, unused. (errcheck
// + ineffassign + staticcheck + unused + govet are part of `default:
// standard` in v2 schema; revive + gosec are explicit `enable:`.)
func TestArch_GolangciLintConfigCanonical(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), ".golangci.yml")
	src, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	body := string(src)
	// v2 schema sentinel.
	if !strings.Contains(body, "version: \"2\"") && !strings.Contains(body, "version: '2'") {
		t.Error(".golangci.yml does not declare version: \"2\" (v2 schema)")
	}
	if !strings.Contains(body, "default: standard") {
		t.Error(".golangci.yml does not enable default standard set (errcheck/govet/ineffassign/staticcheck/unused)")
	}
	required := []string{"revive", "gosec"}
	for _, l := range required {
		if !strings.Contains(body, l) {
			t.Errorf(".golangci.yml does not enable %s", l)
		}
	}
}

// ----------------------------------------------------------------------------
// P2: TestArch_NoNolintWithoutReason
// ----------------------------------------------------------------------------
//
// Every `//nolint:<linter>` directive must be followed by ` // ` +
// rationale on the same line (golangci-lint v2 enforces this with
// `nolintlint`, but the arch test catches early drift).
func TestArch_NoNolintWithoutReason(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	nolintRE := regexp.MustCompile(`//\s*nolint(?::[\w,]+)?(\s|$)`)
	var bad []string

	walkGoFiles(t, filepath.Join(root, "internal"), true, func(path string, src []byte) {
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			if !nolintRE.MatchString(ln) {
				continue
			}
			// Reason is anything after the nolint directive on the
			// same line that begins with `//` AGAIN (i.e. a trailing
			// comment) or after `--` separator.
			tail := nolintRE.Split(ln, 2)
			if len(tail) < 2 {
				continue
			}
			rest := strings.TrimSpace(tail[1])
			// If rest is empty OR is just additional nolint params,
			// no reason was provided.
			if rest == "" || strings.HasPrefix(rest, "//") && len(strings.TrimSpace(strings.TrimPrefix(rest, "//"))) < 5 {
				bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("//nolint directive missing reason comment:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// P3 (TestArch_NoFmtPrintInProduction) — already shipped in
// observability_arch_test.go. Not redeclared here to avoid the
// duplicate symbol error; the original implementation enforces the
// same predicate (no fmt.Print* in internal/ production).

// ----------------------------------------------------------------------------
// P4: TestArch_NoTODOWithoutTicket
// ----------------------------------------------------------------------------
//
// `TODO` / `FIXME` comments without a ticket reference rot
// silently — bare TODOs are aspirational. Linus-rule: every
// TODO carries `(#NNN)` or `(ADR-NNNN)`.
//
// Soft check: budget of 30 raw TODOs total; lower the ceiling as
// the codebase matures.
func TestArch_NoTODOWithoutTicket(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	todoRE := regexp.MustCompile(`(?i)\b(TODO|FIXME)\b[^:]*:?\s*(.*)$`)
	ticketRE := regexp.MustCompile(`(?i)(#\d+|ADR[\s-]?\d+|TRACKED IN|WAVE)`)
	var bad []string

	walkGoFiles(t, filepath.Join(root, "internal"), false, func(path string, src []byte) {
		for i, ln := range strings.Split(string(src), "\n") {
			if !strings.Contains(ln, "//") {
				continue
			}
			comment := ln[strings.Index(ln, "//"):]
			m := todoRE.FindStringSubmatch(comment)
			if m == nil {
				continue
			}
			if ticketRE.MatchString(m[2]) {
				continue
			}
			bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
		}
	})

	const ceiling = 30
	if len(bad) > ceiling {
		t.Fatalf("too many ticketless TODO/FIXME (> %d) — every TODO must cite #NNN or ADR-NNNN:\n  %s",
			ceiling, strings.Join(bad[:20], "\n  "))
	}
}
