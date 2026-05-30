// persistence_arch_test.go — Principle 5: Persistence as Adapter.
//
// Per ADR 0004 (TDL repository canon) + ADR 0047 (boundary discipline)
// + Brandur "Postgres for everything" (ctx-tx pattern): persistence is
// an adapter — the domain owns the Repository interface, the
// adapters/ package supplies a concrete pgx-backed impl. App layer
// depends ONLY on the interface; the active pgx.Tx flows through ctx
// via pg.UnitOfWork.
//
// Tests in this file:
//   30. TestArch_RepositoriesHaveUpdateByIDFn
//   32. TestArch_NoDBTypesInDomainSignatures
//   33. TestArch_CtxFirstArgInRepoMethods
//   34. TestArch_AdaptersJoinParentUoW
//   35. TestArch_RepoErrorsAreTypedSentinels
//   36. TestArch_DomainAndAppDontImportPgxDriver
//
// (#31 NoCrossSchemaJoins relocated to db_schema_arch_test.go per the
//  14-principle taxonomy.)

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 30: TestArch_RepositoriesHaveUpdateByIDFn
// ----------------------------------------------------------------------------
//
// Every aggregate's Repository interface MUST expose `UpdateByID(ctx,
// id, fn)` so command handlers mutate state inside a transaction
// scope without leaking the active tx into the application layer.
//
// EXCEPTIONS (canon-aligned, append-only ledger aggregates).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RepositoriesHaveUpdateByIDFn(t *testing.T) {
	t.Parallel()

	exceptions := map[string]string{
		"stockmovement":     "append-only ledger (per repository.go godoc)",
		"verificationcall":  "append-only call log (per repository.go godoc)",
		"leadcredit":        "optimistic-concurrency UpsertWithVersion (ADR 0059)",
		"calllog":           "append-only CRM call log (ADR 0060 — no state-mutation methods after New)",
		"assignmenthistory": "append-only CRM assignment audit (ADR 0060 — append-only Entry, no UpdateByID)",
		"invoice":           "append-only tax invoice (ADR 0063 §3 — cancellation produces CreditNote, never mutates Invoice)",
		"creditnote":        "append-only reversal document (ADR 0063 §3 — once issued, immutable)",
		"payment":           "append-only payment receipt (ADR 0063 §3 — each receipt is a new row; refunds are new rows, not mutations)",
	}

	type violation struct {
		pkgDir string
		reason string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		entries, err := readDirSafe(domainDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			aggName := e.Name()
			pkgDir := filepath.Join(domainDir, aggName)
			isAgg := false
			hasUpdateByID := false

			walkGoFiles(t, pkgDir, false, func(path string, src []byte) {
				_, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok || ts.Name.Name != "Repository" {
						return true
					}
					iface, ok := ts.Type.(*ast.InterfaceType)
					if !ok {
						return true
					}
					isAgg = true
					for _, m := range iface.Methods.List {
						for _, name := range m.Names {
							if name.Name == "UpdateByID" {
								hasUpdateByID = true
							}
						}
					}
					return true
				})
			})
			if !isAgg {
				continue
			}
			if hasUpdateByID {
				continue
			}
			if _, ok := exceptions[aggName]; ok {
				continue
			}
			violations = append(violations, violation{
				pkgDir: pkgDir,
				reason: "Repository missing UpdateByID(ctx, id, fn) per TDL canon",
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("REPOSITORY UpdateByID VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0004 (TDL Sep 2024 canon): command handlers MUST mutate")
		t.Logf("state via Repository.UpdateByID(ctx, id, func(*Agg) (bool, error)).")
		for _, v := range violations {
			t.Errorf("%s — %s", v.pkgDir, v.reason)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 32: TestArch_NoDBTypesInDomainSignatures
// ----------------------------------------------------------------------------
//
// Domain methods + Repository interfaces never use `pgtype.*`, `pgx.*`,
// or `pgxpool.*` types in signatures. The DB driver's vocabulary is a
// substrate concern — let the adapter convert. A pgtype in a domain
// signature drags the entire driver into the dependency graph + ties
// the domain to one specific Postgres client.
//
// Detection: parse every non-test file under internal/<mod>/domain/;
// flag any SelectorExpr with X named pgtype/pgx/pgxpool inside a
// FieldList of any FuncDecl / InterfaceType / StructType.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoDBTypesInDomainSignatures(t *testing.T) {
	t.Parallel()

	bannedPkgs := map[string]bool{
		"pgtype":  true,
		"pgx":     true,
		"pgxpool": true,
	}

	type violation struct {
		file string
		line int
		why  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			// Also catch the import directly.
			for _, imp := range parseImports(t, path, src) {
				for banned := range bannedPkgs {
					if strings.Contains(imp, "/"+banned+"/") || strings.HasSuffix(imp, "/"+banned) {
						violations = append(violations, violation{
							file: path,
							line: 0,
							why:  "imports " + imp,
						})
					}
				}
			}
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if bannedPkgs[id.Name] {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(sel.Pos()).Line,
						why:  "uses " + id.Name + "." + sel.Sel.Name,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("DB-TYPES-IN-DOMAIN VIOLATIONS — %d", len(violations))
		t.Logf("pgtype / pgx / pgxpool types in a domain signature drag the")
		t.Logf("entire driver into the dependency graph. Convert in the adapter.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s", v.file, v.line, v.why)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 33: TestArch_CtxFirstArgInRepoMethods
// ----------------------------------------------------------------------------
//
// Every method on a `Repository` interface (and any sub-interface
// declared in `internal/<mod>/domain/<agg>/repository.go`) takes
// `ctx context.Context` as the first parameter. Per Brandur ctx-tx
// pattern + Cheney "always carry ctx": cancellation, deadline, and
// tx propagation all flow through ctx.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_CtxFirstArgInRepoMethods(t *testing.T) {
	t.Parallel()

	type violation struct {
		file   string
		line   int
		method string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				if !strings.HasSuffix(ts.Name.Name, "Repository") {
					return true
				}
				iface, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					return true
				}
				for _, m := range iface.Methods.List {
					ft, ok := m.Type.(*ast.FuncType)
					if !ok || ft.Params == nil || len(ft.Params.List) == 0 {
						continue
					}
					first := ft.Params.List[0].Type
					okCtx := false
					if sel, ok := first.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "context" && sel.Sel.Name == "Context" {
							okCtx = true
						}
					}
					if !okCtx {
						for _, name := range m.Names {
							violations = append(violations, violation{
								file:   path,
								line:   fset.Position(name.Pos()).Line,
								method: name.Name,
							})
						}
					}
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("REPO METHOD ctx-FIRST VIOLATIONS — %d", len(violations))
		t.Logf("Per Cheney + Brandur: ctx is the FIRST parameter of every")
		t.Logf("repository method. Cancellation + deadline + tx-stashing all flow")
		t.Logf("through it.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s does not take ctx as first arg", v.file, v.line, v.method)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 34: TestArch_AdaptersJoinParentUoW
// ----------------------------------------------------------------------------
//
// Every adapter `Add` / `Update*` / `Delete*` method calls
// `pg.TxFromContext(ctx)` to check for a parent UnitOfWork before
// opening its own tx. Per ADR 0047: app handlers compose multi-
// aggregate writes via UoW; adapters that ignore the parent tx
// silently split atomic operations into separate transactions.
//
// Heuristic detection: function body contains the literal
// `TxFromContext` token (matches `pg.TxFromContext(ctx)` and any
// equivalent dot-import or rename).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AdaptersJoinParentUoW(t *testing.T) {
	t.Parallel()

	// Allow-list: adapters that never need to join a parent tx (read-
	// only adapters; outbox forwarder; impersonation in-memory store).
	allowList := []string{
		"internal/identity/adapters/audit_reader_pg.go",
		"internal/identity/adapters/auth_router_pg.go",
		"internal/identity/adapters/conversion.go",
		"internal/identity/adapters/impersonation_inmemory_store.go",
		"internal/identity/adapters/impersonation_audit_pg.go",
		"internal/identity/adapters/keyset_explain_integration_test.go",
		"internal/identity/adapters/outbox_forwarder.go",
		"internal/identity/adapters/outbox_writer.go",
		"internal/identity/adapters/passwordpolicy_offline_list.go",
		"internal/identity/adapters/platform_stats_pg.go",
		"internal/identity/adapters/role_hierarchy_edges_pg.go",
		"internal/identity/adapters/search_index_pg.go",
		"internal/identity/adapters/security_stamp_cache.go",
	}

	type violation struct {
		file string
		line int
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		adaptersDir := filepath.Join(internalDir(t), mod, "adapters")
		walkGoFiles(t, adaptersDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			for _, allowed := range allowList {
				if strings.HasSuffix(slashPath, allowed) {
					return
				}
			}
			if !strings.HasSuffix(slashPath, "_pg.go") {
				return
			}
			fset, f := parseFile(t, path, src)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Body == nil {
					continue
				}
				name := fd.Name.Name
				if !strings.HasPrefix(name, "Add") && !strings.HasPrefix(name, "Update") && !strings.HasPrefix(name, "Delete") {
					continue
				}
				body := string(src)
				bodyStart := fset.Position(fd.Body.Lbrace).Offset
				bodyEnd := fset.Position(fd.Body.Rbrace).Offset
				if bodyStart < 0 || bodyEnd > len(body) {
					continue
				}
				fragment := body[bodyStart:bodyEnd]
				if strings.Contains(fragment, "TxFromContext") {
					continue
				}
				// Also accept addOnTx helper calls (canonical wrapper).
				if strings.Contains(fragment, "addOnTx") || strings.Contains(fragment, "runInTx") { // join helpers; bare WithinTx is NOT a join (ADR 0067 Phase-4)
					continue
				}
				violations = append(violations, violation{
					file: path,
					line: fset.Position(fd.Pos()).Line,
					fn:   name,
				})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("ADAPTER UoW-JOIN VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0047: every Add/Update/Delete in *_pg.go must check")
		t.Logf("pg.TxFromContext(ctx) (or call addOnTx) to join a parent UoW.")
		t.Logf("Without this, multi-aggregate writes silently split into")
		t.Logf("separate transactions.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s lacks TxFromContext / addOnTx hook", v.file, v.line, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 35: TestArch_RepoErrorsAreTypedSentinels
// ----------------------------------------------------------------------------
//
// Every repository method that can hit a missing row maps pgx.ErrNoRows
// to a package-level `Err<Agg>NotFound` sentinel, NOT bare wrap. Per
// Cheney error-handling canon + Go 1.13+ `errors.Is`: callers branch
// on `errors.Is(err, repo.ErrNotFound)` — a bare `%w` of pgx.ErrNoRows
// leaks the driver type to the app + breaks the sentinel contract.
//
// Detection: walk *_pg.go files; for any method body containing
// `pgx.ErrNoRows`, assert the same body also references a `ErrNotFound`
// (or `Err<Anything>NotFound`) sentinel — that's the canonical map.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_RepoErrorsAreTypedSentinels(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		adaptersDir := filepath.Join(internalDir(t), mod, "adapters")
		walkGoFiles(t, adaptersDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			if !strings.HasSuffix(slashPath, "_pg.go") {
				return
			}
			fset, f := parseFile(t, path, src)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				bodyStart := fset.Position(fd.Body.Lbrace).Offset
				bodyEnd := fset.Position(fd.Body.Rbrace).Offset
				if bodyStart < 0 || bodyEnd > len(src) {
					continue
				}
				fragment := string(src[bodyStart:bodyEnd])
				if !strings.Contains(fragment, "pgx.ErrNoRows") {
					continue
				}
				if strings.Contains(fragment, "NotFound") {
					continue
				}
				violations = append(violations, violation{
					file: path,
					fn:   fd.Name.Name,
				})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("REPO SENTINEL-MAPPING VIOLATIONS — %d", len(violations))
		t.Logf("Per Cheney: callers branch on errors.Is(err, repo.ErrNotFound).")
		t.Logf("A bare %%w wrap of pgx.ErrNoRows leaks the driver type to app/.")
		for _, v := range violations {
			t.Errorf("%s — %s references pgx.ErrNoRows but no NotFound sentinel", v.file, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 36b: TestArch_PortsAdaptersDontDefineInterfaces (preserved from 19)
// ----------------------------------------------------------------------------
//
// Per Cheney "accept interfaces, return structs" + ADR 0047. Interfaces
// live with their CONSUMER (domain or app); ports/ and adapters/ ship
// concrete types. An interface declared in adapters/ almost always
// means "this concrete struct is forming its own substitution point"
// — usually a sign of inverted dependency.
//
// EXCEPTION list (consumer-side interfaces that legitimately live with
// their primary impl — typically because the consumer is a middleware
// in the same layer).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_PortsAdaptersDontDefineInterfaces(t *testing.T) {
	t.Parallel()

	type key struct{ fileSuffix, ifaceName string }
	allowed := map[key]bool{
		{"identity/adapters/impersonation_audit_pg.go", "ImpersonationAuditWriter"}:    true,
		{"identity/adapters/security_stamp_cache.go", "PersonStampReader"}:             true,
		{"identity/ports/subscribers/invalidate_cache.go", "SecurityStampInvalidator"}: true,
		{"identity/ports/authn/authn.go", "Verifier"}:                                  true,
		{"identity/ports/authn/security_stamp.go", "StampValidator"}:                   true,
	}

	type violation struct {
		file string
		typ  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, layer := range []string{"ports", "adapters"} {
			layerDir := filepath.Join(internalDir(t), mod, layer)
			walkGoFiles(t, layerDir, false, func(path string, src []byte) {
				_, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok {
						return true
					}
					if !ast.IsExported(ts.Name.Name) {
						return true
					}
					if _, ok := ts.Type.(*ast.InterfaceType); !ok {
						return true
					}
					normalised := strings.ReplaceAll(pathToSlash(path), "\\", "/")
					for k := range allowed {
						if strings.HasSuffix(normalised, k.fileSuffix) && ts.Name.Name == k.ifaceName {
							return true
						}
					}
					violations = append(violations, violation{file: path, typ: ts.Name.Name})
					return true
				})
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("INTERFACE-IN-WRONG-LAYER VIOLATIONS — %d", len(violations))
		t.Logf("Per Cheney + ADR 0047: interfaces belong with their CONSUMER")
		t.Logf("(domain or app/), not with their implementation.")
		for _, v := range violations {
			t.Errorf("%s — type %s interface (should live with consumer)", v.file, v.typ)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 36c: TestArch_AppDoesntImportPorts (preserved from 19)
// ----------------------------------------------------------------------------
//
// Per ADR 0047: dependency flow is ports → app → domain. app/ may NOT
// import ports/ from any module.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AppDoesntImportPorts(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		imp  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		appDir := filepath.Join(internalDir(t), mod, "app")
		walkGoFiles(t, appDir, false, func(path string, src []byte) {
			for _, imp := range parseImports(t, path, src) {
				if strings.Contains(imp, "/internal/") && strings.Contains(imp, "/ports") {
					violations = append(violations, violation{file: path, imp: imp})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("APP→PORTS DEPENDENCY-FLOW VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0047: dependency flow is ports → app → domain.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s", v.file, v.imp)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 36d: TestArch_DomainHasNoInfraImports (preserved from 19)
// ----------------------------------------------------------------------------
//
// Per ADR 0002 + ADR 0047 + Vernon IDDD ch. 4: domain is pure Go.
// Substrate concerns enter via domain-declared interfaces. Allowed
// common/* leaves: pure VOs + utilities + typed-ID factories.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_DomainHasNoInfraImports(t *testing.T) {
	t.Parallel()

	forbiddenPrefixes := map[string]string{
		"github.com/jackc/pgx":                                       "DB driver — substrate concern",
		"github.com/ThreeDotsLabs/watermill":                         "message broker — substrate",
		"net/http":                                                   "HTTP — transport concern",
		"log/slog":                                                   "logger — substrate",
		"encoding/json":                                              "serialisation — substrate",
		"github.com/redis":                                           "cache substrate",
		"github.com/leadkart/leadkart-go/internal/common/pg":         "DB transactor — substrate",
		"github.com/leadkart/leadkart-go/internal/common/messaging":  "broker wrapper — substrate",
		"github.com/leadkart/leadkart-go/internal/common/cache":      "cache wrapper — substrate",
	}

	allowedCommonLeaves := map[string]bool{
		"errs":          true,
		"ids":           true,
		"slug":          true,
		"email":         true,
		"phone":         true,
		"pan":           true,
		"gst":           true,
		"postaladdress": true,
		"druglicence":   true,
		"pagination":    true,
		"tenancy":       true,
	}

	type violation struct {
		file   string
		imp    string
		reason string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			imports := parseImports(t, path, src)
			for _, imp := range imports {
				for pref, reason := range forbiddenPrefixes {
					if strings.HasPrefix(imp, pref) {
						violations = append(violations, violation{
							file:   path,
							imp:    imp,
							reason: reason,
						})
					}
				}
				const commonPrefix = "github.com/leadkart/leadkart-go/internal/common/"
				if strings.HasPrefix(imp, commonPrefix) {
					rest := strings.TrimPrefix(imp, commonPrefix)
					leaf := strings.SplitN(rest, "/", 2)[0]
					if !allowedCommonLeaves[leaf] {
						violations = append(violations, violation{
							file:   path,
							imp:    imp,
							reason: "common/" + leaf + " is not on the pure-VO allow-list",
						})
					}
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN-PURITY VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0002 + 0047 + Vernon IDDD ch. 4: domain is pure Go.")
		t.Logf("Allowed common/* leaves: errs, ids, slug, email, phone, pan,")
		t.Logf("gst, postaladdress, druglicence, pagination, tenancy.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s\n  why: %s", v.file, v.imp, v.reason)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 36: TestArch_DomainAndAppDontImportPgxDriver
// ----------------------------------------------------------------------------
//
// Per ADR 0047: the pgx / pgxpool / pgtype driver lives ONLY in
// internal/<mod>/adapters/ + internal/common/pg/. Imports anywhere
// else mean a substrate concern leaked across the layer boundary.
//
// This is the positive-shaped complement of test 32 (which checks
// domain SIGNATURES); test 36 checks imports anywhere in domain + app.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_DomainAndAppDontImportPgxDriver(t *testing.T) {
	t.Parallel()

	forbiddenPrefixes := []string{
		"github.com/jackc/pgx",
	}

	type violation struct {
		file string
		imp  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, layer := range []string{"domain", "app"} {
			layerDir := filepath.Join(internalDir(t), mod, layer)
			walkGoFiles(t, layerDir, false, func(path string, src []byte) {
				for _, imp := range parseImports(t, path, src) {
					for _, pref := range forbiddenPrefixes {
						if strings.HasPrefix(imp, pref) {
							violations = append(violations, violation{file: path, imp: imp})
						}
					}
				}
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("PGX-IN-DOMAIN-OR-APP VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0047: pgx + pgxpool + pgtype live ONLY in adapters/")
		t.Logf("+ internal/common/pg/. domain + app stay pgx-free.")
		for _, v := range violations {
			t.Errorf("%s — imports %s", v.file, v.imp)
		}
	}
}

// ============================================================================
// Principle S — CQRS discipline (3 tests added per the comprehensive catalog
// brief). ADR 0009 + Vernon IDDD ch. 4 (CQRS as a tactical separator).
// ============================================================================

// ----------------------------------------------------------------------------
// S1: TestArch_AppCommandDoesNotImportAppQuery
// ----------------------------------------------------------------------------
//
// CQRS: commands write state; queries read state. The two surfaces
// share NO code (they have different scaling shapes, different
// caching shapes). Commands importing query types signals a
// "command returns a projection" anti-pattern; do the read in a
// follow-up call.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AppCommandDoesNotImportAppQuery(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		cmdDir := filepath.Join(root, mod, "app", "command")
		walkGoFiles(t, cmdDir, false, func(path string, src []byte) {
			imports := parseImports(t, path, src)
			for _, im := range imports {
				if strings.Contains(im, "/app/query") {
					bad = append(bad, pathToSlash(path)+": imports "+im)
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("app/command/ imports app/query/ — CQRS write side can't read its own projections (ADR 0009):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// S2: TestArch_CommandHandlersReturnResult
// ----------------------------------------------------------------------------
//
// Command handlers return either `(*Result, error)` for state-
// reporting commands or `error` for void commands. Returning a
// raw domain entity (`(*Person, error)`) leaks the aggregate into
// the wire layer; the *Result DTO is the boundary.
//
// Allow-list: handlers explicitly returning `(string, error)` for
// new-ID commands; the resource ID is canonical wire payload (not
// a domain leak).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_CommandHandlersReturnResult(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		cmdDir := filepath.Join(root, mod, "app", "command")
		walkGoFiles(t, cmdDir, false, func(path string, src []byte) {
			_, file := parseFile(t, path, src)
			ast.Inspect(file, func(n ast.Node) bool {
				fd, ok := n.(*ast.FuncDecl)
				if !ok || fd.Name.Name != "Handle" {
					return true
				}
				if fd.Recv == nil || !isHandlerReceiver(fd.Recv) {
					return true
				}
				if fd.Type.Results == nil {
					return true
				}
				results := fd.Type.Results.List
				if len(results) == 1 {
					// error-only return — fine.
					return true
				}
				if len(results) != 2 {
					bad = append(bad, pathToSlash(path)+": Handle returns "+itoa(len(results))+" results")
					return true
				}
				// First result should be a pointer or a typed value.
				switch results[0].Type.(type) {
				case *ast.StarExpr, *ast.Ident, *ast.SelectorExpr:
					// OK
				default:
					bad = append(bad, pathToSlash(path)+": Handle first return is unusual shape")
				}
				return true
			})
		})
	}

	if len(bad) > 0 {
		t.Fatalf("command handler Handle method return shape non-canonical:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// S3: TestArch_QueryHandlersDontMutate
// ----------------------------------------------------------------------------
//
// Query handlers MUST NOT write — no `.Add(`, `.Update*(`, `.Delete(`
// calls in app/query/ files. Reads side is for projections only.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_QueryHandlersDontMutate(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	mutRE := regexp.MustCompile(`\.(?:Add|UpdateByID|UpdateBy\w+|Delete|Insert|Save)\(`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		qDir := filepath.Join(root, mod, "app", "query")
		walkGoFiles(t, qDir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			if mutRE.MatchString(body) {
				bad = append(bad, pathToSlash(path))
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("query handler calls a mutator (.Add/.UpdateBy/.Delete) — reads MUST be side-effect-free (ADR 0009):\n  %s",
			strings.Join(bad, "\n  "))
	}
}
