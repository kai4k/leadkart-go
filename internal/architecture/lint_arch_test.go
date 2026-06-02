// lint_arch_test.go — Principle P: Linting / static analysis discipline.
//
// golangci-lint v2 + gosec + revive. Gates: .golangci.yml stays canonical,
// every //nolint: has a reason, TODOs carry ticket references.

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_GolangciLintConfigCanonical asserts .golangci.yml uses v2 schema with
// default: standard and explicit revive + gosec + depguard.
func TestArch_GolangciLintConfigCanonical(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), ".golangci.yml")
	src, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "version: \"2\"") && !strings.Contains(body, "version: '2'") {
		t.Error(".golangci.yml does not declare version: \"2\" (v2 schema)")
	}
	if !strings.Contains(body, "default: standard") {
		t.Error(".golangci.yml does not enable default standard set (errcheck/govet/ineffassign/staticcheck/unused)")
	}
	required := []string{"revive", "gosec", "depguard"}
	for _, l := range required {
		if !strings.Contains(body, l) {
			t.Errorf(".golangci.yml does not enable %s", l)
		}
	}
	if !strings.Contains(body, "github.com/jackc/pgx") {
		t.Error(".golangci.yml depguard does not deny the pgx driver in domain/app (ADR 0047/0066)")
	}
}

// TestArch_NoNolintWithoutReason asserts every //nolint: directive has a
// trailing reason comment.
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

// TestArch_NoTODOWithoutTicket asserts production TODO/FIXME comments cite
// #NNN or ADR-NNNN. Budget: ≤ 30 ticketless TODOs total.
//
// Scope: production — test-file TODOs don't count against the debt ratchet.
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
			idx := strings.Index(ln, "//")
			if idx < 0 {
				continue
			}
			comment := ln[idx:]
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
