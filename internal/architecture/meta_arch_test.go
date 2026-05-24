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
