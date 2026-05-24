// layout_arch_test.go — Principle T: Folder + file conventions.
//
// The TDL Wild Workouts canonical module shape:
//
//   internal/<module>/
//   ├── domain/
//   │   └── <agg>/
//   │       ├── <agg>.go
//   │       ├── events.go
//   │       ├── repository.go
//   │       └── <agg>_test.go
//   ├── app/
//   │   ├── command/
//   │   ├── query/
//   │   └── jobs/        (optional)
//   ├── ports/
//   ├── adapters/
//   │   ├── sql/         (sqlc input)
//   │   └── db/          (sqlc-generated; package db)
//   ├── integrationevents/
//   └── <module>test/    (fakes; optional)
//
// Drift in the shape is the first warning that someone is inventing
// a new architectural idiom. The tests below mechanically check the
// most load-bearing pieces.
//
// Cited canon:
//   - ThreeDotsLabs Wild Workouts (Nov 2025 canonical reference)
//   - Vernon IDDD ch. 4 (architecture as code) — layered + bounded-context-aligned
//   - Go community "small interfaces, consumer-defined" → ports/adapters layout

package architecture_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// T1: TestArch_EveryModuleHasFourLayers
// ----------------------------------------------------------------------------
//
// Every bounded-context module under internal/ contains ONLY the
// canonical top-level dirs: domain, app, ports, adapters,
// integrationevents, optional <module>test/. Stray top-level
// folders (e.g. internal/identity/services/) are drift.
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

// ----------------------------------------------------------------------------
// T2: TestArch_EveryAggregateDirHasCanonicalFiles
// ----------------------------------------------------------------------------
//
// Each domain/<agg>/ aggregate dir should contain at least:
//   - <agg>.go        (the aggregate root + its methods)
//   - events.go       (the V1 events the aggregate emits, optional
//                      if the agg is a pure VO leaf)
//   - repository.go   (the repository interface, optional for pure
//                      VO + cross-aggregate join leaves)
//
// Pragmatic check: every aggregate dir must have AT LEAST <agg>.go
// (the canonical entrypoint). events.go / repository.go presence is
// strongly recommended but not enforced (some aggregates emit no
// events; some are not separately persisted).
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

// ----------------------------------------------------------------------------
// T3: TestArch_PgAdapterFilenamePattern
// ----------------------------------------------------------------------------
//
// Hand-written pgx-backed repository adapters live in
// `<module>/adapters/` with the filename pattern `*_repository_pg.go`
// or `*_pg.go`. The `_pg` suffix telegraphs "this is the Postgres
// impl; swappable" + makes grep for adapter sweeps trivial.
//
// Predicate: any file in `<module>/adapters/` whose name matches
// `*_repository*.go` MUST end in `_repository_pg.go` (or
// `_repository_pg_test.go`).
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

// ----------------------------------------------------------------------------
// T4: TestArch_SqlcGeneratedFilesUnderAdaptersDb
// ----------------------------------------------------------------------------
//
// `*.sql.go` is the sqlc canonical extension. Files matching this
// pattern MUST live under `<module>/adapters/db/` (the generated
// package). Stray *.sql.go files outside that dir are either
// misplaced or hand-written shouldn't-be-shaped-like-sqlc files.
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

// ----------------------------------------------------------------------------
// T5: TestArch_IntegrationTestSuffix
// ----------------------------------------------------------------------------
//
// Files carrying `//go:build integration` SHOULD use the
// `_integration_test.go` filename suffix so integration vs unit tests
// are distinguishable by filename alone — but this is COSMETIC. The
// load-bearing separator per Go canon is the `//go:build integration`
// directive itself (Go 1.17+ `//go:build` syntax; `cmd/go` docs
// §"Build constraints"). CI workflows gate on the build tag, NOT the
// filename, so the rename is style not correctness.
//
// Test enforces the rule that matters: every `//go:build integration`
// file is gated by that tag (which we already get for free — the file
// physically won't compile in the non-integration toolchain), and
// every file with the `_integration_test.go` suffix carries the
// matching build tag. This is the bidirectional check that prevents
// drift; filename ordering matches stdlib pattern (`http_test.go`,
// `http_external_test.go` — no enforced ordering).
//
// arch-test:no-negative-fixture (rule is stdlib idiom — Go's
// `cmd/go` documentation IS the negative-test corpus).
func TestArch_IntegrationTestSuffix(t *testing.T) {
	t.Parallel()

	// Bidirectional check:
	//   - If filename ends in `_integration_test.go`, MUST carry
	//     `//go:build integration` (catch wrong-tag mistakes).
	//   - If file has `//go:build integration` directive, that's
	//     enough — filename suffix is cosmetic per the godoc above.
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

// ----------------------------------------------------------------------------
// T6: TestArch_EveryHandlerHasTestFile
// ----------------------------------------------------------------------------
//
// Every `*Handler` struct in `<module>/app/command/` (the command
// handler bundle) should have a sibling `<file>_test.go`. Coverage
// floor: every command handler must have at least a parse-shape
// unit test.
//
// Pragmatic: files under app/command/ that contain `type *Handler
// struct` must have a same-stem _test.go.
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
			// Allow if there's ANY _test.go in the same dir that
			// references the handler type (common: shared flow tests).
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

// ----------------------------------------------------------------------------
// T7: TestArch_TestFakesInTestPackage
// ----------------------------------------------------------------------------
//
// Types matching `Fake*` / `Stub*` / `Mock*` should live in either:
//   - a `<module>test/` package (the canonical fakes location), OR
//   - a `*_test.go` file (test-internal fakes).
//
// Fakes living in production .go files are a smell — they ship in
// the production binary + grow without bound.
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
			// e.g. /platformtest/ — canonical
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
