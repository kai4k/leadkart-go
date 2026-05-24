// tdl_arch_test.go — TDL Clean Architecture discipline as a CI gate.
//
// Per ADR 0002 (Hexagonal + DDD) + ADR 0004 (TDL repository canon) +
// ADR 0047 (boundary discipline): the four-layer split (domain / app /
// ports / adapters) imposes strict dependency direction + factory +
// repository invariants. These tests turn the ADR text into mechanical
// guards.

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 6: TestArch_DomainHasNoInfraImports
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0002 + ADR 0047 — domain layer is PURE Go. No DB
// driver, no message broker, no HTTP, no logger, no JSON, no cache.
// Per Vernon IDDD ch. 4 + Khorikov §11 + TDL Wild Workouts: the
// domain models the business — it must compile in isolation from
// every substrate. The substrate ports adapt INTO the domain via
// interfaces declared by the domain (Cheney "accept interfaces").
//
// Allowed common/* sub-packages are pure value-objects / utilities
// (no I/O, no globals beyond singleton regexes per common/slug). The
// allow-list below is deliberately closed — new common/<X> packages
// imported from domain require an ADR amendment.
func TestArch_DomainHasNoInfraImports(t *testing.T) {
	t.Parallel()

	// Forbidden prefixes. A domain import is a violation if its path
	// starts with any of these strings.
	forbiddenPrefixes := map[string]string{
		"github.com/jackc/pgx":                              "DB driver — substrate concern; declare a domain Repository interface",
		"github.com/ThreeDotsLabs/watermill":                "message broker — substrate; events leave via repository.PullEvents + outbox",
		"net/http":                                          "HTTP — transport concern; routing lives in ports/",
		"log/slog":                                          "logger — substrate; domain must not log (Khorikov §4: side-effect-free)",
		"encoding/json":                                     "serialisation — substrate; integration-event mapper handles wire concerns",
		"github.com/redis":                                  "cache substrate; declare a domain Reader interface if needed",
		"github.com/leadkart/leadkart-go/internal/common/pg":        "DB transactor — substrate",
		"github.com/leadkart/leadkart-go/internal/common/messaging": "broker wrapper — substrate",
		"github.com/leadkart/leadkart-go/internal/common/cache":     "cache wrapper — substrate",
	}

	// Allowed common/* leaf packages — pure VOs + utilities + ID types.
	// The set is closed; adding to it requires an ADR amendment.
	allowedCommonLeaves := map[string]bool{
		"errs":           true, // sentinel-error factory
		"ids":            true, // UUIDv7 factory
		"slug":           true, // VO
		"email":          true, // VO
		"phone":          true, // VO
		"pan":            true, // VO
		"gst":            true, // VO
		"postaladdress":  true, // VO
		"druglicence":    true, // VO
		"pagination":     true, // generic Page[T] + Cursor — pure
		"tenancy":        true, // tenant.FromContext / ctx-only
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
				// Pass 1: explicit forbidden prefixes.
				for pref, reason := range forbiddenPrefixes {
					if strings.HasPrefix(imp, pref) {
						violations = append(violations, violation{
							file:   path,
							imp:    imp,
							reason: reason,
						})
					}
				}
				// Pass 2: tighten common/* — only allowed-leaves pass.
				const commonPrefix = "github.com/leadkart/leadkart-go/internal/common/"
				if strings.HasPrefix(imp, commonPrefix) {
					rest := strings.TrimPrefix(imp, commonPrefix)
					leaf := strings.SplitN(rest, "/", 2)[0]
					if !allowedCommonLeaves[leaf] {
						violations = append(violations, violation{
							file:   path,
							imp:    imp,
							reason: "common/" + leaf + " is not on the pure-VO allow-list (domain must depend only on pure utilities; declare an interface for anything else)",
						})
					}
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN-PURITY VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0002 + ADR 0047 + Vernon IDDD ch. 4: domain is pure Go.")
		t.Logf("Substrate concerns enter via domain-declared interfaces, never")
		t.Logf("via direct imports. Allowed common/* leaves: errs, ids, slug,")
		t.Logf("email, phone, pan, gst, postaladdress, druglicence, pagination,")
		t.Logf("tenancy.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s\n  why: %s", v.file, v.imp, v.reason)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 7: TestArch_AggregatesHaveFactoryAndUnmarshal
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0004 (TDL repository canon) — every aggregate package
// MUST expose both a `New(...) (*<Agg>, error)` factory (enforces
// invariants on creation) AND an `UnmarshalFromDB(snapshot) *<Agg>`
// re-hydration (repository-only, does NOT re-validate).
//
// Per Wild Workouts canon + Khorikov §11: separating creation from
// re-hydration lets the domain own its invariants while letting the
// repository round-trip immutable, already-validated state.
//
// AGGREGATE DETECTION: a directory is an aggregate iff it declares a
// `type Repository interface`. VOs (leadform) and policy packages
// (permission, passwordpolicy, impersonation) are not aggregates.
//
// CANON-ALIGNED EXCEPTIONS:
//   - refreshtoken: factory is `NewFamily` (the aggregate is
//     refreshtoken.Family, not refreshtoken.Token). Snapshot is
//     `FamilySnapshot`. Accept the canonical Wild Workouts shape
//     when the type name is non-default.
func TestArch_AggregatesHaveFactoryAndUnmarshal(t *testing.T) {
	t.Parallel()

	type violation struct {
		pkgDir   string
		missing  []string
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
			pkgDir := filepath.Join(domainDir, e.Name())
			isAgg := false
			hasFactory := false
			hasUnmarshal := false

			walkGoFiles(t, pkgDir, false, func(path string, src []byte) {
				_, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, isType := n.(*ast.TypeSpec)
					if isType && ts.Name.Name == "Repository" {
						if _, ok := ts.Type.(*ast.InterfaceType); ok {
							isAgg = true
						}
					}
					fd, isFunc := n.(*ast.FuncDecl)
					if !isFunc || fd.Recv != nil {
						return true
					}
					name := fd.Name.Name
					// Accept any exported `New*` factory returning a pointer + error.
					if strings.HasPrefix(name, "New") && returnsPointerAndError(fd) {
						hasFactory = true
					}
					// Accept any `Unmarshal*FromDB` re-hydration.
					if strings.HasPrefix(name, "Unmarshal") && strings.HasSuffix(name, "FromDB") {
						hasUnmarshal = true
					}
					return true
				})
			})

			if !isAgg {
				continue
			}
			var missing []string
			if !hasFactory {
				missing = append(missing, "New<Agg>(...) (*<Agg>, error) factory")
			}
			if !hasUnmarshal {
				missing = append(missing, "Unmarshal[<Type>]FromDB re-hydration")
			}
			if len(missing) > 0 {
				violations = append(violations, violation{pkgDir: pkgDir, missing: missing})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("AGGREGATE FACTORY/UNMARSHAL VIOLATIONS — %d aggregate packages incomplete", len(violations))
		t.Logf("Per ADR 0004 + Wild Workouts canon: every aggregate has TWO entry")
		t.Logf("points — `New` validates invariants on creation; `UnmarshalFromDB`")
		t.Logf("re-hydrates without re-validating. Separating them lets the")
		t.Logf("repository round-trip already-valid state without re-paying the")
		t.Logf("ctor cost on every read (Khorikov §11.4).")
		for _, v := range violations {
			t.Errorf("%s — missing: %v", v.pkgDir, v.missing)
		}
	}
}

// returnsPointerAndError reports whether the function declaration's
// result list is exactly `(*T, error)` for some T.
func returnsPointerAndError(fd *ast.FuncDecl) bool {
	if fd.Type.Results == nil || len(fd.Type.Results.List) != 2 {
		return false
	}
	// First result: *T (StarExpr).
	if _, ok := fd.Type.Results.List[0].Type.(*ast.StarExpr); !ok {
		return false
	}
	// Second result: error.
	id, ok := fd.Type.Results.List[1].Type.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "error"
}

// ----------------------------------------------------------------------------
// Test 8: TestArch_RepositoriesHaveUpdateByIDFn
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0004 (TDL Sep 2024 UpdateByID closure pattern) —
// every aggregate's Repository interface MUST expose `UpdateByID(ctx,
// id, fn)` so command handlers mutate state inside a transaction
// scope without leaking the active tx into the application layer.
//
// EXCEPTIONS (canon-aligned, append-only ledger aggregates):
//   - stockmovement: append-only ledger (per repository godoc + Vernon
//     IDDD on event-stream-ish aggregates).
//   - verificationcall: append-only call log.
//   - leadcredit: optimistic-concurrency UpsertWithVersion replaces
//     UpdateByID (per ADR 0059 §"Optimistic concurrency").
//
// All three exceptions are documented in their Repository interface
// godoc — if you add a fourth, write an ADR.
func TestArch_RepositoriesHaveUpdateByIDFn(t *testing.T) {
	t.Parallel()

	exceptions := map[string]string{
		"stockmovement":     "append-only ledger (per repository.go godoc)",
		"verificationcall":  "append-only call log (per repository.go godoc)",
		"leadcredit":        "optimistic-concurrency UpsertWithVersion (ADR 0059)",
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
		t.Logf("REPOSITORY UpdateByID VIOLATIONS — %d aggregates missing the closure", len(violations))
		t.Logf("Per ADR 0004 (TDL Sep 2024 canon): command handlers MUST mutate")
		t.Logf("state via Repository.UpdateByID(ctx, id, func(*Agg) (bool, error))")
		t.Logf("so transaction scope stays in the adapter, never leaking into app/.")
		t.Logf("Append-only / optimistic-upsert aggregates are exempt — see test godoc.")
		for _, v := range violations {
			t.Errorf("%s — %s", v.pkgDir, v.reason)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 9: TestArch_PortsAdaptersDontDefineInterfaces
// ----------------------------------------------------------------------------
//
// Enforces: Cheney "accept interfaces, return structs" + ADR 0047.
// Interfaces live with their CONSUMER (domain or app); ports/ and
// adapters/ ship concrete types. An interface declared in adapters/
// almost always means "this concrete struct is forming its own
// substitution point" — usually a sign of inverted dependency.
//
// EXCEPTION list (consumer-side interfaces that legitimately live with
// their primary impl — typically because the consumer is a middleware
// in the same layer):
//   - identity/adapters/impersonation_audit_pg.go ImpersonationAuditWriter
//     — consumed by the impersonation HTTP handler in the SAME adapters/
//     bundle (PG + noop swappable impls).
//   - identity/adapters/security_stamp_cache.go PersonStampReader
//     — consumed by the cache adapter itself.
//   - identity/ports/subscribers/invalidate_cache.go SecurityStampInvalidator
//     — consumed by the subscriber inside the same file.
//   - identity/ports/authn/{authn,security_stamp}.go Verifier / StampValidator
//     — consumed by the authn middleware INSIDE ports/authn/ (test
//     fakes substitute against these).
//
// New adapter/port interfaces must either move to domain/app OR be
// added to this exception list with an ADR amendment.
func TestArch_PortsAdaptersDontDefineInterfaces(t *testing.T) {
	t.Parallel()

	// allowedExceptions: full file path (suffix-matched) + interface name.
	type key struct{ fileSuffix, ifaceName string }
	allowed := map[key]bool{
		{"identity/adapters/impersonation_audit_pg.go", "ImpersonationAuditWriter"}: true,
		{"identity/adapters/security_stamp_cache.go", "PersonStampReader"}:          true,
		{"identity/ports/subscribers/invalidate_cache.go", "SecurityStampInvalidator"}: true,
		{"identity/ports/authn/authn.go", "Verifier"}:                                  true,
		{"identity/ports/authn/security_stamp.go", "StampValidator"}:                   true,
	}

	type violation struct {
		file   string
		typ    string
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
					// Suffix-match against allowed exceptions.
					normalised := strings.ReplaceAll(filepath.ToSlash(path), "\\", "/")
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
		t.Logf("Per Cheney 'accept interfaces, return structs' + ADR 0047:")
		t.Logf("interfaces belong with their CONSUMER (domain or app/),")
		t.Logf("not with their implementation. If an interface in ports/ or")
		t.Logf("adapters/ has multiple impls in the same layer, document the")
		t.Logf("rationale + add an entry to this test's allow-list with an ADR.")
		for _, v := range violations {
			t.Errorf("%s — type %s interface (should live with consumer)", v.file, v.typ)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 10: TestArch_AppDoesntImportPorts
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0047 — dependency flow is ports → app → domain.
// app/ may NOT import ports/ from any module. (app/ depending on its
// own module's ports/ inverts the dependency direction; depending on
// another module's ports/ is also forbidden by TestArch_NoCrossModuleImports.)
//
// EXCEPTION: test files (_test.go) are exempt — test fixtures wire
// up integration setups that span layers.
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
				// Catches BOTH same-module and cross-module ports imports.
				if strings.Contains(imp, "/internal/") && strings.Contains(imp, "/ports") {
					violations = append(violations, violation{file: path, imp: imp})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("APP→PORTS DEPENDENCY-FLOW VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0047: dependency flow is ports → app → domain (one-way).")
		t.Logf("app/ depending on ports/ either (a) inverts the flow within the")
		t.Logf("module, or (b) cross-imports another module's transport — the")
		t.Logf("latter is also forbidden by TestArch_NoCrossModuleImports.")
		for _, v := range violations {
			t.Errorf("%s\n  imports: %s", v.file, v.imp)
		}
	}
}
