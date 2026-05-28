// aggregate_invariants_arch_test.go — Principle 3: Aggregate Invariant
// Discipline.
//
// Per Vernon IDDD ch. 4 + ch. 8, Khorikov §11, Wild Workouts canon:
// the aggregate root is the only consistency boundary; all state
// changes flow through the factory/method API; events form a sealed
// closed set; rehydration is silent (does NOT re-validate or re-emit).
//
// Tests in this file:
//   16. TestArch_AggregatesHaveFactoryAndUnmarshal
//   17. TestArch_DomainEventsSealed
//   18. TestArch_AggregatesNoPublicFields
//   19. TestArch_ConstructionOnlyViaFactory
//   20. TestArch_MutatorsEmitEventsOnStateChange
//   21. TestArch_UnmarshalFromDBDoesNotEmitEvents
//   22. TestArch_UnmarshalFromDBDoesNotValidate
//   23. TestArch_AggregateIDsUseTypedIDs

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 16: TestArch_AggregatesHaveFactoryAndUnmarshal
// ----------------------------------------------------------------------------
//
// Every aggregate package MUST expose both a `New(...) (*<Agg>, error)`
// factory (enforces invariants on creation) AND an
// `Unmarshal*FromDB(snapshot) *<Agg>` re-hydration (repository-only,
// does NOT re-validate).
//
// AGGREGATE DETECTION: a directory is an aggregate iff it declares a
// `type Repository interface`. VOs (leadform) and policy packages
// (permission, passwordpolicy, impersonation) are not aggregates.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AggregatesHaveFactoryAndUnmarshal(t *testing.T) {
	t.Parallel()

	type violation struct {
		pkgDir  string
		missing []string
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
					if strings.HasPrefix(name, "New") && returnsPointerAndError(fd) {
						hasFactory = true
					}
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
		t.Logf("re-hydrates without re-validating.")
		for _, v := range violations {
			t.Errorf("%s — missing: %v", v.pkgDir, v.missing)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 17: TestArch_DomainEventsSealed
// ----------------------------------------------------------------------------
//
// Every domain Event interface uses the SEALED marker pattern. Per
// Vernon IDDD ch. 8 + Wild Workouts canon: domain events form a closed
// set per aggregate; an external package adding its own type to the
// marker interface would be a domain-modelling escape hatch. The seal
// is an unexported method (e.g. `isTenantEvent()`) — only types in the
// same package can implement it.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_DomainEventsSealed(t *testing.T) {
	t.Parallel()

	type violation struct {
		file   string
		typ    string
		reason string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			_, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				if ts.Name.Name != "Event" {
					return true
				}
				iface, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					return true
				}
				if iface.Methods == nil || len(iface.Methods.List) == 0 {
					violations = append(violations, violation{
						file:   path,
						typ:    ts.Name.Name,
						reason: "Event interface has no seal method",
					})
					return true
				}
				sealed := false
				for _, m := range iface.Methods.List {
					for _, name := range m.Names {
						if strings.HasPrefix(name.Name, "is") && !ast.IsExported(name.Name) {
							sealed = true
						}
					}
				}
				if !sealed {
					violations = append(violations, violation{
						file:   path,
						typ:    ts.Name.Name,
						reason: "Event interface lacks an unexported seal method",
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("UNSEALED DOMAIN EVENT VIOLATIONS — %d", len(violations))
		t.Logf("Per Vernon IDDD ch. 8: every domain `Event` interface MUST be")
		t.Logf("sealed via an unexported marker method (e.g. `isTenantEvent()`).")
		for _, v := range violations {
			t.Errorf("%s — type %s: %s", v.file, v.typ, v.reason)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 18: TestArch_AggregatesNoPublicFields
// ----------------------------------------------------------------------------
//
// Every aggregate root struct has all fields unexported. External
// access via methods only. Per Vernon IDDD ch. 5: invariants are
// enforced by the aggregate; exposing a field bypasses that.
//
// AGGREGATE DETECTION: same as test 16 — directories that declare
// `type Repository interface`. The aggregate root struct is the
// exported struct with the same name as the package (PascalCase).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AggregatesNoPublicFields(t *testing.T) {
	t.Parallel()

	type violation struct {
		file  string
		line  int
		typ   string
		field string
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
			// First pass: detect aggregate via Repository interface.
			walkGoFiles(t, pkgDir, false, func(path string, src []byte) {
				_, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok || ts.Name.Name != "Repository" {
						return true
					}
					if _, ok := ts.Type.(*ast.InterfaceType); ok {
						isAgg = true
					}
					return true
				})
			})
			if !isAgg {
				continue
			}
			// Second pass: walk every struct in the package; for any
			// exported struct whose name matches the expected aggregate
			// root naming, flag exported fields.
			//
			// Heuristic for "root struct": exported, non-event, non-snapshot.
			// We exclude *Event* + *Snapshot* + obvious VO suffixes.
			walkGoFiles(t, pkgDir, false, func(path string, src []byte) {
				fset, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok {
						return true
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						return true
					}
					name := ts.Name.Name
					if !ast.IsExported(name) {
						return true
					}
					// Skip events, snapshots, DTOs, VOs, options.
					skipSuffixes := []string{"Event", "Snapshot", "Spec", "Options", "DTO", "Dto", "Config", "Settings"}
					for _, s := range skipSuffixes {
						if strings.HasSuffix(name, s) {
							return true
						}
					}
					// Skip if the struct has zero methods (likely a VO).
					// We can't easily tell from a single TypeSpec; rely on
					// the aggregate-root naming heuristic: the struct name
					// must NOT be a verb-noun (e.g. `RoleAssignment`) — too
					// generous. Instead, restrict to exported structs that
					// match the package name (PascalCase).
					pkgName := f.Name.Name
					// PascalCase of pkgName: e.g. tenant -> Tenant, refreshtoken -> Family.
					// We special-case: aggregate root = the exported struct whose
					// name starts with capitalised package or matches the file basename.
					// Fall back to: only flag the EXACT pkgName-cased struct.
					expected := strings.ToUpper(pkgName[:1]) + pkgName[1:]
					if name != expected {
						return true
					}
					// All fields must be unexported.
					for _, field := range st.Fields.List {
						for _, fn := range field.Names {
							if ast.IsExported(fn.Name) {
								violations = append(violations, violation{
									file:  path,
									line:  fset.Position(fn.Pos()).Line,
									typ:   name,
									field: fn.Name,
								})
							}
						}
					}
					return true
				})
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("AGGREGATE PUBLIC FIELD VIOLATIONS — %d", len(violations))
		t.Logf("Per Vernon IDDD ch. 5: aggregate roots enforce invariants.")
		t.Logf("Exported fields bypass the methods that would have validated.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s has exported field %s (must be unexported)", v.file, v.line, v.typ, v.field)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 19: TestArch_ConstructionOnlyViaFactory
// ----------------------------------------------------------------------------
//
// Outside the aggregate's own package, no composite literal
// `<aggPkg>.<AggType>{...}` outside test files. Forces `New<X>` or
// `Unmarshal<X>FromDB` usage.
//
// Detection: parse every non-test .go file; flag composite literals
// whose type is a SelectorExpr `pkg.TypeName` where pkg matches an
// aggregate-package basename AND TypeName matches the canonical
// PascalCase of pkg.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_ConstructionOnlyViaFactory(t *testing.T) {
	t.Parallel()

	// Discover aggregate (pkg, type) pairs.
	aggregates := map[string]string{} // pkg -> exported type
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
			walkGoFiles(t, pkgDir, false, func(path string, src []byte) {
				_, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok || ts.Name.Name != "Repository" {
						return true
					}
					if _, ok := ts.Type.(*ast.InterfaceType); ok {
						isAgg = true
					}
					return true
				})
			})
			if !isAgg {
				continue
			}
			pkgName := e.Name()
			expected := strings.ToUpper(pkgName[:1]) + pkgName[1:]
			aggregates[pkgName] = expected
		}
	}

	type violation struct {
		file string
		line int
		expr string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		// Determine which aggregate package this file belongs to (if any).
		slashPath := pathToSlash(path)
		for pkg, typ := range aggregates {
			// Skip the aggregate's own package (composite literals inside
			// the package are fine — they're the factory's internal use).
			ownPkgFragment := "/domain/" + pkg + "/"
			if strings.Contains(slashPath, ownPkgFragment) {
				continue
			}
			// Parse + AST-walk for composite literals with that selector.
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := cl.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == pkg && sel.Sel.Name == typ {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(cl.Pos()).Line,
						expr: pkg + "." + typ + "{...}",
					})
				}
				return true
			})
		}
	})

	if len(violations) > 0 {
		t.Logf("AGGREGATE-LITERAL-OUTSIDE-PKG VIOLATIONS — %d", len(violations))
		t.Logf("Per Wild Workouts: aggregates are constructed via New<X> or")
		t.Logf("Unmarshal<X>FromDB. A composite literal bypasses the invariants.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s outside the aggregate's own package", v.file, v.line, v.expr)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 20: TestArch_MutatorsEmitEventsOnStateChange
// ----------------------------------------------------------------------------
//
// Heuristic: methods on the aggregate root that return an error (likely
// state mutators) should EITHER call `recordEvent` somewhere in their
// body, OR have a body of < 10 LOC (likely a pure guard / getter).
//
// This catches the "silent state change" bug — a mutation that updates
// fields without enqueuing the corresponding domain event.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_MutatorsEmitEventsOnStateChange(t *testing.T) {
	t.Parallel()

	// Method-name allow-list: state-bearing methods that don't emit
	// events by design (e.g. boolean is-getters that perform a small
	// check).
	allowMethods := map[string]bool{
		"PullEvents":   true, // canonical event-drain method
		"Validate":     true,
		"IsActive":     true,
		"IsDeleted":    true,
		"Equal":        true,
	}

	// Specific aggregate.Method pairs that mutate in-place without
	// recordEvent because the paired ledger aggregate carries the
	// event (Batch + StockMovement co-mutation in inventory) or
	// because the change is structural and tracked via the row's
	// version column.
	type recv struct{ typ, method string }
	allowMethodsPerType := map[recv]bool{
		{"Batch", "ApplyMovement"}:    true, // paired StockMovement emits the inventory.* event
		{"Batch", "SoftDelete"}:       true, // version bump only; deletion auditing via outbox row
		{"Role", "ChangeHierarchyLevel"}: true, // hierarchy edges aggregate owns the event surface (ADR 0058)
		{"Order", "AttachInvoice"}:     true, // event emitted via advance(StateInvoiced, ...) per ADR 0063 §4 state-machine canon
		{"Order", "AttachConsignment"}: true, // event emitted via advance(StateDispatched, ...) per ADR 0063 §4 state-machine canon
	}

	type violation struct {
		file string
		line int
		fn   string
		typ  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			pkgName := f.Name.Name
			expected := strings.ToUpper(pkgName[:1]) + pkgName[1:]
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Body == nil {
					continue
				}
				if allowMethods[fd.Name.Name] {
					continue
				}
				// Receiver must be the aggregate root type.
				rt := fd.Recv.List[0].Type
				if star, ok := rt.(*ast.StarExpr); ok {
					rt = star.X
				}
				id, ok := rt.(*ast.Ident)
				if !ok || id.Name != expected {
					continue
				}
				if allowMethodsPerType[recv{typ: id.Name, method: fd.Name.Name}] {
					continue
				}
				// Return list must include error.
				if fd.Type.Results == nil {
					continue
				}
				hasErr := false
				for _, r := range fd.Type.Results.List {
					if rid, ok := r.Type.(*ast.Ident); ok && rid.Name == "error" {
						hasErr = true
						break
					}
				}
				if !hasErr {
					continue
				}
				// Count LOC (rough — last - first line).
				start := fset.Position(fd.Body.Lbrace).Line
				end := fset.Position(fd.Body.Rbrace).Line
				loc := end - start
				if loc < 10 {
					continue
				}
				// Search body for `recordEvent` call.
				emits := false
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if callName(call.Fun) == "recordEvent" {
						emits = true
						return false
					}
					return true
				})
				if !emits {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(fd.Pos()).Line,
						fn:   fd.Name.Name,
						typ:  expected,
					})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("MUTATOR-WITHOUT-EVENT VIOLATIONS — %d", len(violations))
		t.Logf("Per Vernon IDDD ch. 8 + Wild Workouts canon: aggregate state")
		t.Logf("changes emit domain events via recordEvent. A long-body method")
		t.Logf("returning error that NEVER calls recordEvent is likely a silent")
		t.Logf("mutation (or a hot-spot for missing event coverage).")
		for _, v := range violations {
			t.Errorf("%s:%d — %s.%s returns error + >10 LOC but never calls recordEvent", v.file, v.line, v.typ, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 21: TestArch_UnmarshalFromDBDoesNotEmitEvents
// ----------------------------------------------------------------------------
//
// `Unmarshal*FromDB` bodies must NOT call `recordEvent`. Rehydration
// reconstructs already-validated, already-emitted state — emitting an
// event during rehydration would re-publish historical events on every
// repository read.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_UnmarshalFromDBDoesNotEmitEvents(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := fd.Name.Name
				if !strings.HasPrefix(name, "Unmarshal") || !strings.HasSuffix(name, "FromDB") {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if callName(call.Fun) == "recordEvent" {
						violations = append(violations, violation{
							file: path,
							line: fset.Position(call.Pos()).Line,
							fn:   name,
						})
					}
					return true
				})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("UnmarshalFromDB EMITS EVENTS VIOLATIONS — %d", len(violations))
		t.Logf("Rehydration must be silent. Events are emitted ONLY by")
		t.Logf("factory + mutator methods, never during repository reads.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s calls recordEvent during rehydration", v.file, v.line, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 22: TestArch_UnmarshalFromDBDoesNotValidate
// ----------------------------------------------------------------------------
//
// `Unmarshal*FromDB` bodies must NOT call any function named
// `validate*`. Rehydration trusts the DB — the row was validated on
// insert; re-validating on every read costs CPU + risks rejecting
// historical rows that were valid at write-time but fail a tightened
// rule today.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_UnmarshalFromDBDoesNotValidate(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		fn   string
		call string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := fd.Name.Name
				if !strings.HasPrefix(name, "Unmarshal") || !strings.HasSuffix(name, "FromDB") {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					cn := callName(call.Fun)
					if strings.HasPrefix(cn, "validate") || strings.HasPrefix(cn, "Validate") {
						violations = append(violations, violation{
							file: path,
							line: fset.Position(call.Pos()).Line,
							fn:   name,
							call: cn,
						})
					}
					return true
				})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("UnmarshalFromDB RE-VALIDATES VIOLATIONS — %d", len(violations))
		t.Logf("Rehydration trusts the DB. Validation happens at write-time")
		t.Logf("(in the factory or mutator); re-validating on read costs CPU")
		t.Logf("and risks rejecting historical rows.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s calls %s() during rehydration", v.file, v.line, v.fn, v.call)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 23: TestArch_AggregateIDsUseTypedIDs
// ----------------------------------------------------------------------------
//
// Aggregate root structs' `id` field type is a named type ending in
// `ID` (e.g. `tenant.ID`), not raw `string` or `uuid.UUID`. Per Wild
// Workouts: typed IDs prevent accidental swap (TenantID vs UserID)
// + give the codebase a single place to evolve the underlying repr
// (UUID v4 -> v7 was a one-file change).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AggregateIDsUseTypedIDs(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		typ  string
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
			pkgName := e.Name()
			expected := strings.ToUpper(pkgName[:1]) + pkgName[1:]

			walkGoFiles(t, pkgDir, false, func(path string, src []byte) {
				fset, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok || ts.Name.Name != expected {
						return true
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						return true
					}
					for _, field := range st.Fields.List {
						for _, fn := range field.Names {
							if fn.Name != "id" && fn.Name != "ID" {
								continue
							}
							// Acceptable types: Ident `ID` (in-package), or
							// SelectorExpr `<pkg>.ID`.
							ok := false
							switch t := field.Type.(type) {
							case *ast.Ident:
								if t.Name == "ID" || strings.HasSuffix(t.Name, "ID") {
									ok = true
								}
							case *ast.SelectorExpr:
								if t.Sel.Name == "ID" || strings.HasSuffix(t.Sel.Name, "ID") {
									ok = true
								}
							}
							if !ok {
								violations = append(violations, violation{
									file: path,
									line: fset.Position(field.Pos()).Line,
									typ:  expected,
								})
							}
						}
					}
					return true
				})
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("AGGREGATE-ID TYPED-ID VIOLATIONS — %d", len(violations))
		t.Logf("Per Wild Workouts: aggregate `id` is a named type (e.g.")
		t.Logf("`tenant.ID`), not raw string / uuid.UUID. Typed IDs prevent")
		t.Logf("cross-aggregate ID swaps + isolate repr changes.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s.id is not a typed ID (use named type ending in `ID`)", v.file, v.line, v.typ)
		}
	}
}
