// tdl_strict_arch_test.go — STRICT TDL canon fitness functions.
//
// These tests close the specific gaps that allowed parallel patterns to
// coexist in LeadKart-Go during ADR 0062's "explicit-tenantID push"
// (and the earlier drifts that motivated ADRs 0047, 0062, this file).
// Each TestArch_TDL* below codifies one position from
// docs/doctrine/tdl_canon.md. Read that doc for the WHY; this file is
// the mechanical gate.
//
// Why "strict" as a separate file? The existing arch suite is broad +
// general-doctrine; this file pins TDL-SPECIFIC takes that diverge
// from generic Clean Architecture (fakes-not-mocks, no-service-layer,
// no-ctx-smuggled-domain-values, repository-not-per-use-case). When a
// future PR weakens one of these, the failure message points at
// docs/doctrine/tdl_canon.md so the reviewer can audit the trade-off
// against the canonical source rather than the local interpretation.
//
// FAILURE MODES CAUGHT (each maps 1:1 to a Test* below):
//
//   - tenancy.FromContext leaking into app/ → ctx-smuggled tenant ID
//     in parallel with explicit parameter (the ADR 0062 drift).
//   - validate:"…" struct tags on domain or Command/Query types →
//     business validation in struct metadata instead of aggregate
//     methods (the "Single Model" anti-pattern).
//   - Repository.Save / Repository.Upsert methods → hides the
//     creation-vs-mutation distinction the domain has to answer.
//   - Repository methods named after business verbs (Cancel*, Approve*,
//     Reschedule*) → drags business logic out of the aggregate.
//   - Mock-generation tools (mockery/gomock/testify/mock) imported in
//     production tests → snapshot mocks freeze interface evolution.
//   - Handler types with Handle methods deviating from the canonical
//     (ctx, X) error / (ctx, X) (Y, error) shape.
//   - `service/` directory under internal/<module>/ containing handler
//     types or business logic (TDL allows service/ only as the
//     composition root: NewApplication + cleanup).
//
// arch-test:parallel-safe-file — every Test* below uses read-only AST
//   walks; no shared mutable state.

package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// TestArch_TDL_NoTenancyFromContextInApp
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §4): context.Context carries TRANSPORT metadata
// only (deadlines, cancellation, request-id, trace span). Domain values
// — tenant.ID, aggregate.ID, business state — flow as EXPLICIT
// parameters on commands / queries / Repository methods.
//
// The bug class: two code paths that both "know the current tenant"
// disagree, and the discrepancy is invisible at the call site. The
// ADR 0062 refactor (Phase 7) hit this exact failure — ctx-GUC and
// explicit-parameter tenant transport both ran for months until one
// audit noticed they diverged for a single membership lookup. With
// explicit parameters, the mismatch is a compile error.
//
// The boundary extraction happens at ports/ middleware (which calls
// tenancy.FromContext to pull the JWT-projected tenant from the request)
// and at internal/common/pg/ (which binds the ID into the SQL GUC).
// Everything in between (app/ command + query handlers, domain/) MUST
// pass tenant.ID as an explicit function parameter.
//
// Scope: production — applies to non-test files only. Test fixtures
// freely construct ctx with tenancy.WithID, so test-side discipline is
// out of scope here (and would produce only false positives).
//
// arch-test:no-negative-fixture — guards the live tree.
func TestArch_TDL_NoTenancyFromContextInApp(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		appDir := filepath.Join(root, mod, "app")
		walkGoFiles(t, appDir, false, func(path string, src []byte) {
			text := string(src)
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if !strings.Contains(line, "tenancy.FromContext") {
					continue
				}
				if strings.Contains(line, "// arch-test:tdl-ctx-tenancy-allowed") {
					continue
				}
				violations = append(violations, violation{
					file: pathToSlash(path),
					line: i + 1,
				})
			}
		})
	}

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	t.Errorf("%d tenancy.FromContext call(s) inside app/ — forbidden per TDL canon §4:", len(violations))
	t.Logf("  Position: domain values (tenant.ID) flow as EXPLICIT parameters,")
	t.Logf("  not via context.Context. ports/ middleware does the boundary")
	t.Logf("  extraction; app/ handlers receive cmd.TenantID / q.TenantID.")
	t.Logf("  Reference: docs/doctrine/tdl_canon.md §4, ADR 0062.")
	for _, v := range violations {
		t.Logf("  %s:%d", v.file, v.line)
	}
}

// ----------------------------------------------------------------------------
// TestArch_TDL_NoValidateTagsInDomain
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §11): business invariants live in aggregate
// constructors + mutators, NOT in struct tags. The
// `validate:"…"` go-playground/validator tags are the
// transport-layer's tool — they belong on DTO types under ports/, not
// on domain entities.
//
// Bug class: business rules encoded in struct tags are invisible on
// any code path that bypasses the tag-validating adapter (subscriber
// dispatch, internal admin tooling, future event-replay flows).
// Putting validation in domain methods means EVERY construction path
// is gated by the same rules.
//
// Scope: production — applies to non-test files only. Test fixtures
// may construct synthetic structs for negative-fixture validation
// of the rule itself.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_NoValidateTagsInDomain(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	validateTagRE := regexp.MustCompile("`[^`]*validate:\"[^\"]+\"[^`]*`")

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(root, mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			text := stripGoComments(string(src))
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if validateTagRE.MatchString(line) {
					violations = append(violations, violation{
						file: pathToSlash(path),
						line: i + 1,
					})
				}
			}
		})
	}

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	t.Errorf("%d validate:\"…\" tag(s) inside domain/ — forbidden per TDL canon §11:", len(violations))
	t.Logf("  Position: business invariants live in aggregate ctors + mutators,")
	t.Logf("  not struct tags. validate: tags belong on ports/ DTO types only.")
	t.Logf("  Reference: docs/doctrine/tdl_canon.md §11.")
	for _, v := range violations {
		t.Logf("  %s:%d", v.file, v.line)
	}
}

// ----------------------------------------------------------------------------
// TestArch_TDL_NoValidateTagsOnCommandsOrQueries
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §11): Command + Query structs are plain data
// passed handler-to-handler — neither validator tags nor business
// checks belong on them. Translation chain: DTO (ports/, may have
// validate: tags) → Command/Query (app/, no tags) → Domain methods
// (validation lives here).
//
// Scope: production — applies to non-test files only. Same rationale
// as TestArch_TDL_NoValidateTagsInDomain.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_NoValidateTagsOnCommandsOrQueries(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	validateTagRE := regexp.MustCompile("`[^`]*validate:\"[^\"]+\"[^`]*`")

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		appDir := filepath.Join(root, mod, "app")
		walkGoFiles(t, appDir, false, func(path string, src []byte) {
			text := stripGoComments(string(src))
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if validateTagRE.MatchString(line) {
					violations = append(violations, violation{
						file: pathToSlash(path),
						line: i + 1,
					})
				}
			}
		})
	}

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	t.Errorf("%d validate:\"…\" tag(s) inside app/ — forbidden per TDL canon §11:", len(violations))
	t.Logf("  Position: Command + Query structs carry plain data only.")
	t.Logf("  Business validation lives in domain methods; structural")
	t.Logf("  validation lives on the DTO at the HTTP boundary.")
	for _, v := range violations {
		t.Logf("  %s:%d", v.file, v.line)
	}
}

// repositoryExceptions are the documented per-aggregate exceptions to
// the canonical Add/Get/UpdateByID + persistence-verb shape, kept in
// lockstep with persistence_arch_test.go's exception list. New entries
// require an ADR + a godoc note on the Repository interface.
var repositoryExceptions = map[string]string{
	"leadcredit":        "optimistic-concurrency UpsertWithVersion (ADR 0059)",
	"stockmovement":     "append-only ledger",
	"verificationcall":  "append-only call log",
	"calllog":           "append-only CRM call log (ADR 0060)",
	"assignmenthistory": "append-only CRM assignment audit (ADR 0060)",
	"platformlead":      "MarketplaceBrowse — Stripe-style filter+keyset list (ADR 0060)",
}

func repoIsExempted(repoFilePath string) (string, bool) {
	for aggName, reason := range repositoryExceptions {
		if strings.Contains(pathToSlash(repoFilePath), "/domain/"+aggName+"/") {
			return reason, true
		}
	}
	return "", false
}

// ----------------------------------------------------------------------------
// TestArch_TDL_RepositoryHasNoSaveOrUpsertMethod
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §3): Repository interfaces expose Add (new
// aggregate) + Get (load) + UpdateByID (closure-based mutation). A
// Save / Upsert method hides the creation-vs-mutation question the
// domain has to answer — and removes the natural place to enforce the
// "is this a new aggregate or an existing one?" branch.
//
// Aggregates whose nature genuinely demands upsert (per-tenant counters
// with optimistic concurrency — see ADR 0059) are listed in
// repositoryExceptions. New entries require an ADR.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_RepositoryHasNoSaveOrUpsertMethod(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type violation struct {
		file   string
		method string
	}
	var violations []violation

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) != "repository.go" {
			return nil
		}
		slash := pathToSlash(path)
		if !strings.Contains(slash, "/domain/") {
			return nil
		}
		if _, exempt := repoIsExempted(path); exempt {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "Repository" {
					continue
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				for _, m := range it.Methods.List {
					for _, n := range m.Names {
						lower := strings.ToLower(n.Name)
						if lower == "save" || lower == "upsert" ||
							strings.HasPrefix(lower, "save") ||
							strings.HasPrefix(lower, "upsert") {
							violations = append(violations, violation{
								file:   slash,
								method: n.Name,
							})
						}
					}
				}
			}
		}
		return nil
	})

	if len(violations) == 0 {
		return
	}

	t.Errorf("%d Save/Upsert method(s) on Repository interface(s) — forbidden per TDL canon §3:", len(violations))
	t.Logf("  Position: Repository = Add (new) + Get (load) + UpdateByID")
	t.Logf("  (closure-based mutation). Save/Upsert collapses the")
	t.Logf("  creation-vs-mutation distinction the domain must answer.")
	for _, v := range violations {
		t.Logf("  %s — method %s", v.file, v.method)
	}
}

// ----------------------------------------------------------------------------
// TestArch_TDL_RepositoryHasNoBusinessVerbMethods
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §3): Repository methods are persistence verbs
// (Add, Get, List, UpdateByID). Business verbs (Cancel*, Approve*,
// Reschedule*, Suspend*, etc.) live as AGGREGATE methods passed via
// the UpdateByID closure. The ddd-cqrs-clean-architecture-combined
// post explicitly refactors per-use-case repo methods back into
// UpdateTraining with a closure.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_RepositoryHasNoBusinessVerbMethods(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	// Whitelist of persistence verbs that may appear as method prefixes.
	// Anything else (Cancel, Approve, Suspend, Activate, etc.) is a
	// business verb and belongs on the aggregate, not the repository.
	persistencePrefixes := []string{
		"Add", "Get", "List", "Update", "Delete", "Has", "Count",
		"Find", "Search", "Hard", "Exists", "Any",
	}

	type violation struct {
		file   string
		method string
	}
	var violations []violation

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) != "repository.go" {
			return nil
		}
		slash := pathToSlash(path)
		if !strings.Contains(slash, "/domain/") {
			return nil
		}
		if _, exempt := repoIsExempted(path); exempt {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "Repository" {
					continue
				}
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				for _, m := range it.Methods.List {
					for _, n := range m.Names {
						if !methodHasPersistencePrefix(n.Name, persistencePrefixes) {
							violations = append(violations, violation{
								file:   slash,
								method: n.Name,
							})
						}
					}
				}
			}
		}
		return nil
	})

	if len(violations) == 0 {
		return
	}

	t.Errorf("%d Repository method(s) named after business verbs — forbidden per TDL canon §3:", len(violations))
	t.Logf("  Position: Repository methods are persistence verbs (Add, Get,")
	t.Logf("  List, UpdateByID). Business verbs (Cancel/Approve/Suspend/etc.)")
	t.Logf("  live as aggregate methods, called inside the UpdateByID closure.")
	t.Logf("  Allowed method prefixes: %s", strings.Join(persistencePrefixes, ", "))
	for _, v := range violations {
		t.Logf("  %s — method %s", v.file, v.method)
	}
}

func methodHasPersistencePrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// TestArch_TDL_NoMockGenerationTools
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §6): "All Pub/Sub implementations are passing
// the same test suite" — TDL prefers FAKES (faithful in-memory impls
// that pass the same contract test the SQL adapter passes) over mocks
// (snapshot-frozen call expectations). Mock-generation tools couple
// test maintenance to interface evolution + lose the "behaviour-
// equivalent" property fakes give.
//
// Allow `testify/require` + `testify/assert` (assertion sugar, not
// mocks). Forbid `testify/mock`, `gomock`, `mockery`-generated stubs.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_NoMockGenerationTools(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	forbidden := map[string]string{
		"github.com/stretchr/testify/mock":     "testify/mock — use a hand-written FakeRepository per TDL canon §6",
		"go.uber.org/mock":                     "gomock — use a hand-written FakeRepository per TDL canon §6",
		"go.uber.org/mock/gomock":              "gomock — use a hand-written FakeRepository per TDL canon §6",
		"github.com/golang/mock/gomock":        "gomock — use a hand-written FakeRepository per TDL canon §6",
		"github.com/vektra/mockery":            "mockery — use a hand-written FakeRepository per TDL canon §6",
	}

	type violation struct {
		file   string
		importPath string
	}
	var violations []violation

	walkGoFiles(t, root, true, func(path string, src []byte) {
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if perr != nil {
			return
		}
		for _, imp := range f.Imports {
			pathLit := strings.Trim(imp.Path.Value, `"`)
			if _, banned := forbidden[pathLit]; banned {
				violations = append(violations, violation{
					file:   pathToSlash(path),
					importPath: pathLit,
				})
			}
		}
	})

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		return violations[i].file < violations[j].file
	})
	t.Errorf("%d mock-generation tool import(s) — forbidden per TDL canon §6:", len(violations))
	t.Logf("  Position: TDL prefers FAKES (faithful contract impls in")
	t.Logf("  <aggregate>test/) over mocks (snapshot-frozen call expectations).")
	t.Logf("  Reference: docs/doctrine/tdl_canon.md §6.")
	for _, v := range violations {
		t.Logf("  %s — imports %q (%s)", v.file, v.importPath, forbidden[v.importPath])
	}
}

// ----------------------------------------------------------------------------
// TestArch_TDL_HandlerSignatureStrict
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §5): every command/query handler exposes
// EXACTLY ONE exported Handle method with one of two signatures:
//
//	Handle(ctx context.Context, cmd X) error
//	Handle(ctx context.Context, q X) (Y, error)
//
// No "HandleAsync", no overloaded Handle*ByThing variants, no
// SignatureSugar (`Cancel(ctx, id)`). The signature uniformity is
// what lets generic decorators / middleware wrap every handler.
//
// Scope: production — applies to non-test files only. Tests construct
// handler instances + invoke Handle directly; they do not declare new
// Handler types.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_HandlerSignatureStrict(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type violation struct {
		file   string
		typ    string
		issue  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, sub := range []string{"command", "query"} {
			dir := filepath.Join(root, mod, "app", sub)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				fset := token.NewFileSet()
				f, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
				if perr != nil {
					return
				}
				// Collect Handler types declared in this file.
				handlerTypes := map[string]bool{}
				for _, decl := range f.Decls {
					gd, ok := decl.(*ast.GenDecl)
					if !ok {
						continue
					}
					for _, spec := range gd.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						if !strings.HasSuffix(ts.Name.Name, "Handler") {
							continue
						}
						handlerTypes[ts.Name.Name] = true
					}
				}
				if len(handlerTypes) == 0 {
					return
				}
				// Track which handler types have a Handle method.
				hasHandle := map[string]bool{}
				for _, decl := range f.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
						continue
					}
					recvType := receiverTypeName(fd.Recv.List[0].Type)
					if !handlerTypes[recvType] {
						continue
					}
					if fd.Name.Name != "Handle" {
						continue
					}
					hasHandle[recvType] = true
					if !handlerHasCanonicalSignature(fd) {
						violations = append(violations, violation{
							file:  pathToSlash(path),
							typ:   recvType,
							issue: "Handle signature deviates from Handle(ctx, X) error / Handle(ctx, X) (Y, error)",
						})
					}
				}
				// Handlers with no Handle method at all are also a
				// violation (the type isn't a usable handler).
				for typ := range handlerTypes {
					if !hasHandle[typ] {
						violations = append(violations, violation{
							file:  pathToSlash(path),
							typ:   typ,
							issue: "no exported Handle method",
						})
					}
				}
			})
		}
	}

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].typ < violations[j].typ
	})
	t.Errorf("%d handler signature violation(s) — per TDL canon §5:", len(violations))
	t.Logf("  Position: every *Handler exposes Handle(ctx, X) error")
	t.Logf("  OR Handle(ctx, X) (Y, error). Uniformity lets generic")
	t.Logf("  decorators wrap every handler.")
	for _, v := range violations {
		t.Logf("  %s — %s: %s", v.file, v.typ, v.issue)
	}
}

func receiverTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func handlerHasCanonicalSignature(fd *ast.FuncDecl) bool {
	// Params: exactly (ctx, X)
	if fd.Type.Params == nil || fd.Type.Params.NumFields() != 2 {
		return false
	}
	first := fd.Type.Params.List[0]
	if first.Type == nil {
		return false
	}
	if !typeIsContextContext(first.Type) {
		return false
	}
	// Results: exactly (error) or (X, error)
	if fd.Type.Results == nil {
		return false
	}
	switch fd.Type.Results.NumFields() {
	case 1:
		return typeIsError(fd.Type.Results.List[0].Type)
	case 2:
		return typeIsError(fd.Type.Results.List[1].Type)
	}
	return false
}

func typeIsContextContext(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == "context" && sel.Sel.Name == "Context"
}

func typeIsError(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "error"
}

// ----------------------------------------------------------------------------
// TestArch_TDL_NoServiceDirInModules
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §5 + §12): TDL uses `internal/<module>/service/`
// ONLY as the module composition root — a single file `service.go`
// exporting `NewApplication(ctx) (app.Application, cleanup func())`.
// Business logic, handlers, or use-case code under `service/` is the
// anti-pattern the "no service layer" rule guards against.
//
// LeadKart currently puts module composition in cmd/api/main.go (a
// known deviation; documented in tdl_canon.md §12 — either follow TDL
// or accept the deviation explicitly). This test enforces the
// downstream rule regardless: no `service/` directory under
// `internal/<module>/` at all. If we later adopt the TDL pattern, this
// test will need a narrow exception for the single composition-root
// file.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_NoServiceDirInModules(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type violation struct {
		dir string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		serviceDir := filepath.Join(root, mod, "service")
		info, err := os.Stat(serviceDir)
		if err != nil || !info.IsDir() {
			continue
		}
		violations = append(violations, violation{dir: pathToSlash(serviceDir)})
	}

	if len(violations) == 0 {
		return
	}

	t.Errorf("%d internal/<module>/service/ director(ies) — forbidden per TDL canon §5/§12:", len(violations))
	t.Logf("  Position: no 'service layer' between handlers and domain.")
	t.Logf("  TDL uses service/ ONLY for the composition root")
	t.Logf("  (NewApplication + cleanup). LeadKart's composition lives")
	t.Logf("  in cmd/api/main.go — service/ dirs serve no canon purpose.")
	for _, v := range violations {
		t.Logf("  %s", v.dir)
	}
}

// ----------------------------------------------------------------------------
// TestArch_TDL_NoSetterMethodsOnAggregates
// ----------------------------------------------------------------------------
//
// Position (tdl_canon.md §2): aggregates expose BEHAVIOUR methods
// ("ScheduleTraining()"), not setters ("SetAvailability(state)").
// Setters let any caller put the aggregate in any state regardless of
// business rules — defeating the "always-valid-in-memory" rule the
// ctor enforces.
//
// Heuristic: scan domain/<aggregate>/*.go (excluding _test.go); flag
// exported methods on aggregate-typed receivers whose name begins with
// "Set". Allowlist via an `// arch-test:tdl-setter-allowed reason:…`
// comment on the method (for genuine domain verbs that happen to
// start with "Set" — e.g. "SetUp" as part of a multi-word verb).
//
// Scope: production — applies to non-test files only. Domain aggregate
// methods are declared in production .go files; tests don't extend
// aggregate APIs.
//
// arch-test:no-negative-fixture.
func TestArch_TDL_NoSetterMethodsOnAggregates(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type violation struct {
		file   string
		method string
		line   int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(root, mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, src, parser.ParseComments)
			if perr != nil {
				return
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil {
					continue
				}
				name := fd.Name.Name
				if !ast.IsExported(name) {
					continue
				}
				// True setter pattern: "Set" followed by an uppercase
				// letter. `Settings()` / `Setup()` are NOT setters
				// (they're nouns/verbs that happen to start with the
				// letters "Set").
				if !looksLikeSetter(name) {
					continue
				}
				// Allow `// arch-test:tdl-setter-allowed` next to method.
				if hasNearbyDirective(f, fd, "tdl-setter-allowed") {
					continue
				}
				violations = append(violations, violation{
					file:   pathToSlash(path),
					method: name,
					line:   fset.Position(fd.Pos()).Line,
				})
			}
		})
	}

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	t.Errorf("%d Set* method(s) on domain aggregates — forbidden per TDL canon §2:", len(violations))
	t.Logf("  Position: aggregates expose behaviour methods, not setters.")
	t.Logf("  Setters bypass invariants; use domain verbs (Schedule, Cancel,")
	t.Logf("  Approve, etc.) instead.")
	t.Logf("  Allowlist directive: // arch-test:tdl-setter-allowed reason:…")
	for _, v := range violations {
		t.Logf("  %s:%d — %s", v.file, v.line, v.method)
	}
}

// looksLikeSetter reports whether name matches the true setter
// pattern: "Set" followed by an uppercase letter. Excludes nouns +
// verbs that happen to begin with the letters "Set" (Settings, Setup,
// Setpoint, etc.).
func looksLikeSetter(name string) bool {
	if !strings.HasPrefix(name, "Set") || len(name) < 4 {
		return false
	}
	c := name[3]
	return c >= 'A' && c <= 'Z'
}

// hasNearbyDirective returns true if the comment immediately preceding
// the given declaration contains the directive (after the `// arch-test:`
// prefix).
func hasNearbyDirective(_ *ast.File, fd *ast.FuncDecl, directive string) bool {
	if fd.Doc != nil {
		for _, c := range fd.Doc.List {
			if hasArchTestDirective(c.Text, directive) {
				return true
			}
		}
	}
	return false
}
