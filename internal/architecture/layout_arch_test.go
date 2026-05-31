// layout_arch_test.go — Principle T: Folder + file conventions.
//
// TDL Wild Workouts canonical module shape: domain/<agg>/, app/command/,
// app/query/, ports/, adapters/sql+db/, integrationevents/, <module>test/.
// Drift signals an invented architectural idiom.

package architecture_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestArch_EveryModuleHasFourLayers asserts every module has only canonical
// top-level dirs: domain, app, ports, adapters, integrationevents, <module>test/.
func TestArch_EveryModuleHasFourLayers(t *testing.T) {
	t.Parallel()

	canonical := map[string]struct{}{
		"domain":            {},
		"app":               {},
		"ports":             {},
		"adapters":          {},
		"integrationevents": {},
	}

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		modDir := filepath.Join(root, mod)
		entries, err := readDirSafe(modDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if _, ok := canonical[name]; ok {
				continue
			}
			// <module>test/ is the canonical fakes location.
			if name == mod+"test" {
				continue
			}
			bad = append(bad, mod+"/"+name)
		}
	}

	if len(bad) > 0 {
		t.Fatalf("non-canonical top-level dir under internal/<module>/ (TDL Wild Workouts):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_EveryAggregateDirHasCanonicalFiles asserts every domain/<agg>/ dir
// contains at least one non-test .go file.
func TestArch_EveryAggregateDirHasCanonicalFiles(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(root, mod, "domain")
		entries, err := readDirSafe(domainDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			aggName := e.Name()
			aggDir := filepath.Join(domainDir, aggName)
			subEntries, _ := readDirSafe(aggDir)
			// Accept ANY .go file (non-test) as the entry-point. The
			// project convention is loose — aggregates name their
			// root-shape file by purpose (family.go for refreshtoken,
			// checker.go for passwordpolicy, etc.).
			hasGo := false
			for _, sub := range subEntries {
				if sub.IsDir() {
					continue
				}
				if strings.HasSuffix(sub.Name(), ".go") &&
					!strings.HasSuffix(sub.Name(), "_test.go") {
					hasGo = true
					break
				}
			}
			if !hasGo {
				bad = append(bad, mod+"/domain/"+aggName)
			}
		}
	}

	if len(bad) > 0 {
		t.Fatalf("aggregate dir contains no non-test .go file (empty package):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_PgAdapterFilenamePattern asserts *_repository*.go files in adapters/
// follow the *_repository_pg.go convention.
func TestArch_PgAdapterFilenamePattern(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		adapterDir := filepath.Join(root, mod, "adapters")
		entries, err := readDirSafe(adapterDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.Contains(name, "_repository") {
				continue
			}
			if !strings.HasSuffix(name, "_repository_pg.go") &&
				!strings.HasSuffix(name, "_repository_pg_test.go") &&
				!strings.HasSuffix(name, "_repository_pg_integration_test.go") &&
				// EXPLAIN-under-RLS test gates (ADR 0038) — the file
				// EXPLAINs a query routed through the repository; it is
				// not itself the adapter. Same dir for proximity.
				!strings.HasSuffix(name, "_repository_explain_integration_test.go") {
				bad = append(bad, mod+"/adapters/"+name)
			}
		}
	}

	if len(bad) > 0 {
		t.Fatalf("repository adapter doesn't follow *_repository_pg.go convention:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_SqlcGeneratedFilesUnderAdaptersDb asserts *.sql.go files only
// exist under <module>/adapters/db/.
func TestArch_SqlcGeneratedFilesUnderAdaptersDb(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		modDir := filepath.Join(root, mod)
		walkGoFiles(t, modDir, true, func(path string, _ []byte) {
			if !strings.HasSuffix(path, ".sql.go") {
				return
			}
			if !strings.Contains(pathToSlash(path), "/adapters/db/") {
				bad = append(bad, pathToSlash(path))
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("*.sql.go file outside <module>/adapters/db/ — relocate or rename:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_IntegrationTestSuffix asserts *_integration_test.go files carry
// //go:build integration. Bidirectional: suffix ↔ build tag.
//
// arch-test:no-negative-fixture (rule is stdlib idiom).
func TestArch_IntegrationTestSuffix(t *testing.T) {
	t.Parallel()

	var mismatched []string
	walkGoFiles(t, internalDir(t), true, func(path string, src []byte) {
		body := string(src)
		hasTag := strings.Contains(body, "//go:build integration")
		hasSuffix := strings.HasSuffix(pathToSlash(path), "_integration_test.go")
		if hasSuffix && !hasTag {
			mismatched = append(mismatched, pathToSlash(path)+" — filename promises integration, lacks //go:build integration")
		}
	})

	if len(mismatched) > 0 {
		t.Errorf("filename / build-tag mismatch — every `*_integration_test.go` file MUST carry `//go:build integration` (Go 1.17+ `//go:build` syntax; cmd/go docs §Build constraints):")
		for _, m := range mismatched {
			t.Logf("  %s", m)
		}
	}
}

// TestArch_EveryHandlerHasTestFile asserts every *Handler struct in
// app/command/ has a sibling *_test.go or is referenced from one.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_EveryHandlerHasTestFile(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "app", "command")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			if !strings.Contains(body, "Handler struct") {
				return
			}
			testPath := strings.TrimSuffix(path, ".go") + "_test.go"
			if _, err := readFileBytes(testPath); err == nil {
				return
			}
			handlerName := ""
			for _, ln := range strings.Split(body, "\n") {
				if i := strings.Index(ln, "Handler struct"); i > 0 {
					before := strings.TrimSpace(ln[:i])
					if j := strings.LastIndex(before, " "); j >= 0 {
						handlerName = before[j+1:] + "Handler"
					} else {
						handlerName = before + "Handler"
					}
					break
				}
			}
			if handlerName == "" {
				return
			}
			covered := false
			entries, _ := readDirSafe(dir)
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				src, err := readFileBytes(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				if strings.Contains(string(src), handlerName) {
					covered = true
					break
				}
			}
			if !covered {
				bad = append(bad, pathToSlash(path)+" → no test refs "+handlerName)
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("command handler has no test coverage (no _test.go file references it):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_TestFakesInTestPackage asserts Fake*/Stub*/Mock* types live in
// <module>test/ or _test.go files, not production code.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_TestFakesInTestPackage(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.HasSuffix(slash, "_test.go") {
			return
		}
		if strings.Contains(slash, "test/") {
			return
		}
		body := stripGoComments(string(src))
		for _, ln := range strings.Split(body, "\n") {
			ln = strings.TrimSpace(ln)
			if !strings.HasPrefix(ln, "type ") {
				continue
			}
			rest := strings.TrimPrefix(ln, "type ")
			fields := strings.Fields(rest)
			if len(fields) < 1 {
				continue
			}
			name := fields[0]
			if strings.HasPrefix(name, "Fake") || strings.HasPrefix(name, "Stub") ||
				strings.HasPrefix(name, "Mock") {
				bad = append(bad, slash+": "+name)
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("Fake*/Stub*/Mock* type in production file (move to <module>test/ or _test.go):\n  %s",
			strings.Join(bad, "\n  "))
	}
}
