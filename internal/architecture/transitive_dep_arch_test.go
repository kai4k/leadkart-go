// transitive_dep_arch_test.go — Principle: layer boundaries hold through
// the FULL dependency closure, not just direct imports.
//
// The other layer-boundary gates (TestArch_AppDoesNotImportForbidden per
// ADR 0047, TestArch_NoCrossModuleImports) match DIRECT import statements.
// That misses the ".NET transitive NuGet leak" class: app → someHelper →
// pgx slips past a direct-import scan. This gate walks the transitive
// closure via `go list -deps` and fails on a forbidden package anywhere in
// it. ADR 0066.
//
// Rules (deliberately FP-free against the legitimate substrate seam):
//
//   - domain/  must not transitively reach pgx (driver), adapters/db
//     (sqlc rows), or any concrete adapters package. The model stays
//     persistence-ignorant all the way down.
//   - app/     must not transitively reach adapters/db or any concrete
//     adapters package. NOTE: app legitimately reaches pgx *transitively*
//     via internal/common/pg (the pg.UnitOfWork interface bridges the
//     driver), so pgx is NOT banned for app — only the generated rows and
//     concrete adapters are.
//
// arch-test:no-negative-fixture — RED→GREEN proof is the mutation test
// (plant a pgx import in a domain pkg, or an adapters/db import in app →
// RED; revert → GREEN). A committed testdata fixture would need a real
// forbidden import that would itself fail the build.
//
// arch-test:no-synctest — shells out to `go list`; no goroutines/time/DB.
package architecture_test

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// goListPackage is the subset of `go list -json` output we read.
type goListPackage struct {
	ImportPath string
	Deps       []string // full transitive import closure
}

func TestArch_LayersHaveNoForbiddenTransitiveDeps(t *testing.T) {
	t.Parallel()

	const (
		pgx        = "github.com/jackc/pgx" // covers pgx, pgxpool, pgtype (all under /v5)
		adaptersDB = "/adapters/db"
		adapters   = "/adapters"
	)

	// `go list -json` populates each package's .Deps with the full
	// transitive import closure by default.
	cmd := exec.CommandContext(t.Context(), "go", "list", "-json", "./internal/...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("go list start: %v", err)
	}

	type violation struct {
		pkg    string
		layer  string
		forbid string
	}
	var violations []violation

	dec := json.NewDecoder(out)
	for {
		var p goListPackage
		if derr := dec.Decode(&p); derr == io.EOF {
			break
		} else if derr != nil {
			t.Fatalf("decode go list json: %v", derr)
		}

		ip := p.ImportPath
		// Skip test-helper subpackages' irrelevance: they're classified by
		// path below. Only domain/ and app/ carry rules.
		var layer string
		var forbidden []string
		switch {
		case strings.Contains(ip, "/domain/") || strings.HasSuffix(ip, "/domain"):
			layer = "domain"
			forbidden = []string{pgx, adaptersDB, adapters}
		case strings.Contains(ip, "/app/") || strings.HasSuffix(ip, "/app"):
			layer = "app"
			forbidden = []string{adaptersDB, adapters}
		default:
			continue
		}

		for _, dep := range p.Deps {
			for _, f := range forbidden {
				if strings.Contains(dep, f) {
					violations = append(violations, violation{pkg: ip, layer: layer, forbid: dep})
					break // one violation per dep; adapters/db also matches /adapters
				}
			}
		}
	}
	if werr := cmd.Wait(); werr != nil {
		t.Fatalf("go list: %v", werr)
	}

	if len(violations) > 0 {
		t.Errorf("%d transitive layer-boundary leak(s) — a forbidden package appears in the dependency closure (ADR 0066/0047):", len(violations))
		for _, v := range violations {
			t.Errorf("  [%s] %s\n        transitively imports %s", v.layer, v.pkg, v.forbid)
		}
	}
}
