// producer_consumer_arch_test.go — three fitness functions that
// structurally catch mistake classes which shipped (or nearly shipped)
// this session and which the EXISTING gates could not see:
//
//   GATE 1. TestArch_TopicProducerConsumerBijection
//           — the headline: producer-emitted topics ↔ consumer-filtered
//             topics must agree. The prior magic-string gates
//             (TestArch_TopicNamingConvention, TestArch_EventTypeFilterUsesConstant,
//             TestArch_MessageMetadataUsesHeaderConstants) each check ONE
//             side in isolation — naming shape of producers, or "consumer
//             must use a const not a literal". None of them cross the two
//             sides. The underscore/hyphen drift escaped precisely because
//             a producer↔consumer AGREEMENT violation is a bijection
//             property, not a one-sided shape property.
//
//   GATE 2. TestArch_MigrationDoesNotReferenceLaterSchema
//           — a migration must not reference a schema created by a LATER
//             migration. Caught previously only when the full migration
//             set was applied to a real Postgres in Docker (a cloud-CI
//             round-trip, minutes later). This gate catches it statically
//             at PR time in milliseconds.
//
//   GATE 3. TestArch_ErrgroupClosuresNoSharedWrite
//           — an errgroup g.Go closure that ASSIGNS to a free variable
//             captured from the enclosing function (without a mutex /
//             atomic) is a data race when ≥2 siblings run concurrently.
//             This is the search.go class (since fixed to per-goroutine
//             locals composed after g.Wait()). The race detector only
//             fires if the racy path is actually exercised under -race;
//             this gate makes the shape itself a compile-time-adjacent
//             failure.
//
// arch-test:no-synctest — all three are purely-static AST/text analysis;
// no goroutines, no time-bound, no DB.

package architecture_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ============================================================================
// GATE 1 — TestArch_TopicProducerConsumerBijection
// ============================================================================
//
// PRODUCED set = every string a `Topic()` method under
// internal/*/integrationevents/ returns (the literal value, or the value
// of the const it returns).
//
// CONSUMED set = every topic a subscriber under internal/*/ports/subscribers/
// filters on, i.e. the right-hand side X of a comparison
// `msg.Metadata.Get(<…>HeaderEventType) {==,!=} X`. X is resolved to its
// constant VALUE: it is virtually always a reference to a producer's
// exported Topic const (TestArch_EventTypeFilterUsesConstant already bans
// the string-literal form), so we resolve const references against the
// union of ALL topic-constant VALUES declared anywhere under
// integrationevents/.
//
// ASSERT: CONSUMED ⊆ PRODUCED. A consumed topic with no producer (a
// dangling subscriber) — or a const whose value doesn't match any
// produced Topic() string (the underscore/hyphen drift) — fails with a
// message naming the consumer file + the offending topic.
//
// Orphan PRODUCERS (a Topic() with no consumer yet) are ALLOWED — not
// every event has a subscriber. They're logged as info only.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (flip platform LeadPurchasedV1.Topic()
// to a hyphen variant → this gate goes RED). A sibling fixture file with
// a dangling consumed topic would itself be the banned shape under the
// path-anchored walk; the bijection matcher IS the fitness function.
//
// arch-test:no-synctest — purely-static analysis test.
//
// Scope: production — producers + subscriber filters live in production
// integrationevents/ + ports/subscribers/ code; test envelopes construct
// topics freely.
func TestArch_TopicProducerConsumerBijection(t *testing.T) {
	t.Parallel()

	// 1) Collect every topic-constant VALUE declared anywhere under
	//    integrationevents/, keyed by the const NAME, so a consumer that
	//    references the const by selector (pkg.TopicX) resolves to its
	//    value.
	constValueByName := map[string]string{} // "TopicLeadPurchasedV1" -> "platform.lead_purchased.v1"

	// 2) PRODUCED = every value returned by a Topic() method. A Topic()
	//    body either returns a string literal directly, or returns a const
	//    identifier whose value we already captured in constValueByName.
	produced := map[string]string{}                // topic value -> source location (file)
	producedByTypeName := map[string]string{}      // "LeadPurchasedV1" -> "platform.lead_purchased.v1"
	topicMethodReturnsConst := map[string]string{} // file:typeName -> const name (for the deferred resolve)
	topicMethodTypeAtKey := map[string]string{}    // file:typeName -> receiver type name

	for _, mod := range modulesUnderInternal(t) {
		ieDir := filepath.Join(internalDir(t), mod, "integrationevents")
		walkGoFiles(t, ieDir, false, func(path string, src []byte) {
			_, f := parseFile(t, path, src)
			// Pass A: const decls (string-valued).
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						val := strings.Trim(lit.Value, "`\"")
						constValueByName[name.Name] = val
					}
				}
			}
			// Pass B: Topic() methods. Their return is either a string
			// literal (record the value) or a const ident (defer resolve).
			// Key each by the file + receiver type name so we can map the
			// produced value back to the event TYPE (consumers filter via
			// `<Type>{}.Topic()`, so type-name resolution is the load-bearing
			// path).
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Name.Name != "Topic" || fd.Recv == nil || fd.Body == nil || len(fd.Recv.List) == 0 {
					continue
				}
				rt := fd.Recv.List[0].Type
				if star, ok := rt.(*ast.StarExpr); ok {
					rt = star.X
				}
				typeName := ""
				if id, ok := rt.(*ast.Ident); ok {
					typeName = id.Name
				}
				key := pathToSlash(path) + ":" + typeName
				topicMethodTypeAtKey[key] = typeName
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					ret, ok := n.(*ast.ReturnStmt)
					if !ok || len(ret.Results) != 1 {
						return true
					}
					switch r := ret.Results[0].(type) {
					case *ast.BasicLit:
						if r.Kind == token.STRING {
							val := strings.Trim(r.Value, "`\"")
							produced[val] = pathToSlash(path)
							if typeName != "" {
								producedByTypeName[typeName] = val
							}
						}
					case *ast.Ident:
						topicMethodReturnsConst[key] = r.Name
					case *ast.SelectorExpr:
						// return pkg.TopicX — rare in same package; record name.
						topicMethodReturnsConst[key] = r.Sel.Name
					}
					return true
				})
			}
		})
	}
	// Resolve Topic()-returns-const into produced values (by both value
	// and the receiving event type name).
	for key, constName := range topicMethodReturnsConst {
		val, ok := constValueByName[constName]
		if !ok {
			continue
		}
		produced[val] = strings.SplitN(key, ":", 2)[0]
		if typeName := topicMethodTypeAtKey[key]; typeName != "" {
			producedByTypeName[typeName] = val
		}
	}

	if len(produced) == 0 {
		t.Fatal("no produced topics discovered under integrationevents/ — repo layout drift?")
	}

	// 3) CONSUMED = the X in `…Get(…HeaderEventType) {==,!=} X` inside
	//    ports/subscribers/. Resolve X (ident or selector) to its const
	//    value; a bare string literal is handled by
	//    TestArch_EventTypeFilterUsesConstant, but if one appears here we
	//    take its literal value directly so the bijection still applies.
	type consumed struct {
		file  string
		line  int
		topic string // resolved value, or "<unresolved:Name>"
	}
	var consumedList []consumed

	getsEventType := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Get" {
			return false
		}
		switch a := call.Args[0].(type) {
		case *ast.SelectorExpr:
			return a.Sel.Name == "HeaderEventType"
		case *ast.Ident:
			return a.Name == "HeaderEventType"
		}
		return false
	}

	// resolveTopicExpr maps a topic-bearing expression to its produced
	// value. The consumer idioms it must crack (all present in the repo):
	//
	//   "literal"                              → the literal
	//   pkg.TopicConst / TopicConst            → constValueByName lookup
	//   pkg.SomeEventV1{}.Topic()              → producedByTypeName[SomeEventV1]
	//   expected (a local var)                 → nameToTopic[expected] (pre-scanned)
	//   LeadPurchasedTopic (a local const ALIAS = pkg.TopicConst) → nameToTopic
	//
	// nameToTopic is the per-file pre-scan of every identifier (const or
	// var) whose RHS resolves to a topic value.
	//nolint:staticcheck // S1021: recursive closure — the name must be
	// declared before the literal can reference itself; cannot be merged.
	var resolveTopicExpr func(expr ast.Expr, nameToTopic map[string]string) (string, bool)
	resolveTopicExpr = func(expr ast.Expr, nameToTopic map[string]string) (string, bool) {
		switch x := expr.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				return strings.Trim(x.Value, "`\""), true
			}
		case *ast.Ident:
			if v, ok := nameToTopic[x.Name]; ok {
				return v, true
			}
			if v, ok := constValueByName[x.Name]; ok {
				return v, true
			}
			return "<unresolved:" + x.Name + ">", true
		case *ast.SelectorExpr:
			if v, ok := constValueByName[x.Sel.Name]; ok {
				return v, true
			}
			return "<unresolved:" + x.Sel.Name + ">", true
		case *ast.CallExpr:
			// <CompositeLit>.Topic() — the canonical filter idiom.
			if v, ok := topicFromCompositeLitDotTopic(x, producedByTypeName); ok {
				return v, true
			}
		}
		return "", false
	}

	for _, mod := range modulesUnderInternal(t) {
		subDir := filepath.Join(internalDir(t), mod, "ports", "subscribers")

		// Collect every file in the subscriber PACKAGE first — const aliases
		// (e.g. CRM's `LeadPurchasedTopic = platformevents.TopicLeadPurchasedV1`)
		// frequently live in a sibling file from the comparison that uses
		// them, so nameToTopic must be built package-wide, not per-file.
		type subFile struct {
			path string
			fset *token.FileSet
			f    *ast.File
		}
		var files []subFile
		walkGoFiles(t, subDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			files = append(files, subFile{path: pathToSlash(path), fset: fset, f: f})
		})
		if len(files) == 0 {
			continue
		}

		// Pre-scan (package-wide): build nameToTopic from every const decl +
		// any `name := <expr>` / `name = <expr>` whose RHS resolves to a
		// topic (the `expected := Event{}.Topic()` and `Topic = pkg.Const`
		// aliases). Iterate to a fixed point so alias-of-alias resolves.
		nameToTopic := map[string]string{}
		scan := func() bool {
			changed := false
			record := func(name string, rhs ast.Expr) {
				if name == "" || name == "_" {
					return
				}
				if _, done := nameToTopic[name]; done {
					return
				}
				if v, ok := resolveTopicExpr(rhs, nameToTopic); ok && !strings.HasPrefix(v, "<unresolved:") {
					nameToTopic[name] = v
					changed = true
				}
			}
			for _, sf := range files {
				ast.Inspect(sf.f, func(n ast.Node) bool {
					switch d := n.(type) {
					case *ast.GenDecl:
						if d.Tok != token.CONST && d.Tok != token.VAR {
							return true
						}
						for _, spec := range d.Specs {
							vs, ok := spec.(*ast.ValueSpec)
							if !ok {
								continue
							}
							for i, nm := range vs.Names {
								if i < len(vs.Values) {
									record(nm.Name, vs.Values[i])
								}
							}
						}
					case *ast.AssignStmt:
						if len(d.Lhs) == len(d.Rhs) {
							for i, lhs := range d.Lhs {
								if id, ok := lhs.(*ast.Ident); ok {
									record(id.Name, d.Rhs[i])
								}
							}
						}
					}
					return true
				})
			}
			return changed
		}
		for scan() { //nolint:revive // fixed-point alias resolution
		}

		for _, sf := range files {
			ast.Inspect(sf.f, func(n ast.Node) bool {
				bin, ok := n.(*ast.BinaryExpr)
				if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
					return true
				}
				var other ast.Expr
				switch {
				case getsEventType(bin.X):
					other = bin.Y
				case getsEventType(bin.Y):
					other = bin.X
				default:
					return true
				}
				if topic, ok := resolveTopicExpr(other, nameToTopic); ok {
					consumedList = append(consumedList, consumed{
						file:  sf.path,
						line:  sf.fset.Position(bin.Pos()).Line,
						topic: topic,
					})
				}
				return true
			})
		}
	}

	// 4) ASSERT CONSUMED ⊆ PRODUCED.
	type violation struct {
		file  string
		line  int
		topic string
		why   string
	}
	var violations []violation
	for _, c := range consumedList {
		if strings.HasPrefix(c.topic, "<unresolved:") {
			violations = append(violations, violation{
				file: c.file, line: c.line, topic: c.topic,
				why: "consumed topic constant could not be resolved to a value declared under integrationevents/ — the subscriber filters on a const this gate can't find a producer Topic() for",
			})
			continue
		}
		if _, ok := produced[c.topic]; !ok {
			violations = append(violations, violation{
				file: c.file, line: c.line, topic: c.topic,
				why: "consumed topic has NO producer Topic() under integrationevents/ — producer/consumer drift (e.g. underscore↔hyphen) or a dangling subscriber",
			})
		}
	}

	// Info: orphan producers (allowed).
	consumedValues := map[string]bool{}
	for _, c := range consumedList {
		consumedValues[c.topic] = true
	}
	var orphans []string
	for topic, file := range produced {
		if !consumedValues[topic] {
			orphans = append(orphans, topic+"  ("+file+")")
		}
	}
	slices.Sort(orphans)
	if len(orphans) > 0 {
		t.Logf("PRODUCED \\ CONSUMED — %d orphan producer topic(s) with no subscriber yet (allowed):", len(orphans))
		for _, o := range orphans {
			t.Logf("  %s", o)
		}
	}

	if len(violations) > 0 {
		t.Errorf("%d subscriber topic filter(s) violate the producer↔consumer bijection (CONSUMED ⊄ PRODUCED) — the one-sided magic-string gates can't see a two-sided AGREEMENT violation, which is why the underscore/hyphen topic drift escaped:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — consumes %q — %s", v.file, v.line, v.topic, v.why)
		}
	}
}

// topicFromCompositeLitDotTopic resolves the `<Type>{}.Topic()` /
// `pkg.<Type>{}.Topic()` consumer filter idiom to the produced topic
// value, by mapping the composite-literal's TYPE name through
// producedByTypeName.
func topicFromCompositeLitDotTopic(call *ast.CallExpr, producedByTypeName map[string]string) (string, bool) {
	// call must be `<X>.Topic()` with zero args.
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Topic" || len(call.Args) != 0 {
		return "", false
	}
	// sel.X must be a composite literal `Type{}` or `pkg.Type{}`.
	cl, ok := sel.X.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	typeName := ""
	switch ty := cl.Type.(type) {
	case *ast.Ident:
		typeName = ty.Name
	case *ast.SelectorExpr:
		typeName = ty.Sel.Name
	}
	if typeName == "" {
		return "", false
	}
	if v, ok := producedByTypeName[typeName]; ok {
		return v, true
	}
	return "", false
}

// ============================================================================
// GATE 2 — TestArch_MigrationDoesNotReferenceLaterSchema
// ============================================================================
//
// Parse migrations/*.sql in filename order. Build the set of app schemas
// created so far via `CREATE SCHEMA [IF NOT EXISTS] <name>`. For each
// migration, find every `<schema>.<object>` reference where <schema> is
// one of the KNOWN app schemas (the union of every CREATE SCHEMA name
// seen across ALL migrations — so we never flag pg_catalog /
// information_schema / column aliases). ASSERT: no migration references a
// schema that a LATER migration creates.
//
// False-positive guards:
//   - strip `--` line comments + `/* */` block comments;
//   - strip single-quoted string literals (so GUC names like
//     'app.tenant_id' inside current_setting(...) don't read as schema
//     refs — DDL/DML never qualifies a real object inside a string);
//   - only schema qualifiers in the known-app-schema set are considered;
//   - same-migration references are fine (the CREATE is in the same file).
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (add `CREATE INDEX … ON platform.outbox`
// to identity_init → RED). A committed fixture migration referencing a
// later schema would itself be the broken-migration shape this gate
// exists to ban; the ordered-scan matcher IS the fitness function.
//
// arch-test:no-synctest — purely-static analysis test.
func TestArch_MigrationDoesNotReferenceLaterSchema(t *testing.T) {
	t.Parallel()

	migs := loadMigrations(t)
	// loadMigrations returns os.ReadDir order = lexical = timestamp-prefix
	// order. Sort defensively so the gate doesn't depend on FS ordering.
	slices.SortFunc(migs, func(a, b struct {
		path string
		text string
	}) int {
		return strings.Compare(filepath.Base(a.path), filepath.Base(b.path))
	})

	createSchemaRE := regexp.MustCompile(`(?i)\bCREATE\s+SCHEMA\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)`)
	// schema.object — schema is the captured qualifier; object is any ident.
	qualRefRE := regexp.MustCompile(`\b(\w+)\.\w+`)

	// Pass 1: discover the universe of app schemas + the migration index
	// (0-based, in order) at which each is first CREATEd.
	knownSchema := map[string]bool{}
	createdAt := map[string]int{}
	for i, m := range migs {
		body := stripSQLLiterals(stripSQLComments(m.text))
		for _, sm := range createSchemaRE.FindAllStringSubmatch(body, -1) {
			name := strings.ToLower(sm[1])
			knownSchema[name] = true
			if _, seen := createdAt[name]; !seen {
				createdAt[name] = i
			}
		}
	}

	if len(knownSchema) == 0 {
		t.Fatal("no CREATE SCHEMA found across migrations/ — gate can't establish the known-schema set")
	}

	// Pass 2: for each migration, every reference to a known app schema
	// must be to one created at or before this migration's index.
	type violation struct {
		file        string
		schema      string
		createdFile string
	}
	var violations []violation
	for i, m := range migs {
		body := stripSQLLiterals(stripSQLComments(m.text))
		reported := map[string]bool{} // dedup per (file, schema)
		for _, rm := range qualRefRE.FindAllStringSubmatch(body, -1) {
			schema := strings.ToLower(rm[1])
			if !knownSchema[schema] {
				continue // not an app schema — pg_catalog, alias, GUC ns, etc.
			}
			created, ok := createdAt[schema]
			if !ok {
				continue
			}
			if created > i && !reported[schema] {
				reported[schema] = true
				violations = append(violations, violation{
					file:        filepath.Base(m.path),
					schema:      schema,
					createdFile: filepath.Base(migs[created].path),
				})
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("%d migration(s) reference a schema created by a LATER migration — the broken-migration class that only a full Docker apply caught before; this fails it statically at PR time:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s references schema %q created later by %s", v.file, v.schema, v.createdFile)
		}
	}
}

// stripSQLLiterals blanks the contents of single-quoted SQL string
// literals (preserving newlines for line arithmetic) so that GUC names
// and other dotted text inside string args don't read as schema.object
// references. Doubled single-quotes (”) are the SQL escape for a literal
// quote; we treat them naively (the heuristic only needs to drop dotted
// identifiers, and the doubled-quote case still blanks the bytes between).
func stripSQLLiterals(s string) string {
	var b strings.Builder
	inLit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			inLit = !inLit
			b.WriteByte(' ')
			continue
		}
		if inLit {
			if c == '\n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ============================================================================
// GATE 3 — TestArch_ErrgroupClosuresNoSharedWrite
// ============================================================================
//
// AST-walk every non-test file under internal/*/app/**. For each function
// that creates an errgroup (imports golang.org/x/sync/errgroup), collect
// its `g.Go(func() error {…})` closures and, per closure, the set of FREE
// variables it ASSIGNS — variables declared in the enclosing function
// scope and captured by reference.
//
// SOUND RULE — flag a free variable only when ≥2 SIBLING closures of the
// same errgroup write it (without a mutex/atomic). That is the actual data
// race: two concurrent goroutines mutating the same shared cell. A simpler
// "flag ANY free-var write" rule was REJECTED because it false-positives on
// the canonical fixed shape (search.go: closure A writes only `persons`,
// closure B writes only `tenants` — disjoint cells, no race). The
// ≥2-writers rule is both sound (no repo-wide false positives) and useful
// (the reintroduced defect — BOTH closures writing the same `view.HasPartial`
// — puts one var in 2 closures → flagged). Tuning reported in the deliverable.
//
// SAFE patterns that explicitly do NOT flag:
//   - per-goroutine DISJOINT free vars (search.go's fixed shape);
//   - per-goroutine locals declared INSIDE the closure, composed after Wait;
//   - channels / return-and-assemble-after-Wait;
//   - indexed-slice writes with distinct CONSTANT indices (results[0]=…,
//     results[1]=… — the canonical fan-in; disjoint cells);
//   - any closure that takes a sync.Mutex.Lock / uses sync/atomic.
//
// search.go is already FIXED (disjoint per-goroutine locals composed after
// Wait), so this gate is GREEN now.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (reintroduce a shared `view.HasPartial
// = true` write from both closures in search.go → RED). A committed
// fixture file with a racy closure would itself be the banned shape; the
// AST free-variable-assignment matcher IS the fitness function.
//
// arch-test:no-synctest — purely-static analysis test.
//
// Scope: production — errgroup fan-out lives in production app/ query
// handlers; test files may share state under their own discipline.
func TestArch_ErrgroupClosuresNoSharedWrite(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		name string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		appDir := filepath.Join(internalDir(t), mod, "app")
		walkGoFiles(t, appDir, false, func(path string, src []byte) {
			imports := parseImports(t, path, src)
			usesErrgroup := false
			for _, imp := range imports {
				if imp == "golang.org/x/sync/errgroup" {
					usesErrgroup = true
					break
				}
			}
			if !usesErrgroup {
				return
			}
			fset, f := parseFile(t, path, src)

			ast.Inspect(f, func(n ast.Node) bool {
				outer, ok := n.(*ast.FuncDecl)
				if !ok || outer.Body == nil {
					return true
				}
				// Variables declared at the enclosing-function scope (params,
				// receiver, named results, and `:=` / var decls in the body).
				// An ASSIGNMENT to one of these inside a g.Go closure is a
				// FREE (captured-by-reference) write.
				enclosing := collectEnclosingVars(outer)

				// Per closure: the set of free variables it writes (unguarded)
				// + the line of the first such write (for diagnostics).
				type closureWrites struct {
					vars map[string]int // free var -> first write line
				}
				var closures []closureWrites

				ast.Inspect(outer.Body, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Go" || len(call.Args) != 1 {
						return true
					}
					lit, ok := call.Args[0].(*ast.FuncLit)
					if !ok || lit.Body == nil {
						return true
					}
					closureLocals := collectClosureLocals(lit)
					guarded := closureUsesMutexOrAtomic(lit)
					cw := closureWrites{vars: map[string]int{}}

					ast.Inspect(lit.Body, func(k ast.Node) bool {
						as, ok := k.(*ast.AssignStmt)
						if !ok {
							return true
						}
						for _, lhs := range as.Lhs {
							base, isIndexedConst := assignTargetBase(lhs)
							if base == "" {
								continue
							}
							// Indexed write with a CONSTANT index to a free slice
							// is the disjoint-cell fan-in pattern — safe.
							if isIndexedConst {
								continue
							}
							if closureLocals[base] {
								continue // closure-local, not free
							}
							if !enclosing[base] {
								continue // not an enclosing-scope var (e.g. pkg-level)
							}
							if guarded {
								continue // mutex/atomic guards the write
							}
							if _, seen := cw.vars[base]; !seen {
								cw.vars[base] = fset.Position(as.Pos()).Line
							}
						}
						return true
					})
					closures = append(closures, cw)
					return true
				})

				// SOUND RULE — flag a free variable only when ≥2 SIBLING
				// closures of the same errgroup write it. That is the actual
				// data race: two goroutines mutating the same shared cell
				// concurrently. DISJOINT per-goroutine writes (search.go's
				// fixed shape — closure A writes `persons`, closure B writes
				// `tenants`) are NOT a race and do NOT flag. The reintroduced
				// defect (BOTH closures writing the same `view.HasPartial`)
				// has the same var in ≥2 closures → flagged. This keeps the
				// gate green repo-wide while still catching the mutation.
				writerCount := map[string]int{}
				firstLine := map[string]int{}
				for _, cw := range closures {
					for v, ln := range cw.vars {
						writerCount[v]++
						if _, ok := firstLine[v]; !ok {
							firstLine[v] = ln
						}
					}
				}
				for v, count := range writerCount {
					if count >= 2 {
						violations = append(violations, violation{
							file: pathToSlash(path),
							line: firstLine[v],
							name: v,
						})
					}
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Errorf("%d enclosing-scope variable(s) written by ≥2 SIBLING errgroup g.Go closures without a mutex/atomic — concurrent goroutines racing on the same shared cell (the search.go data-race class). Give each goroutine its own local + compose after g.Wait(), use a channel, or a distinct constant slice index:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — free variable %q written by multiple concurrent g.Go closures", v.file, v.line, v.name)
		}
	}
}

// collectEnclosingVars returns the set of identifier names introduced at
// the enclosing function's own scope: receiver, params, named results,
// and any `:=` / `var` declarations in the function body that sit OUTSIDE
// a g.Go FuncLit. (Declarations inside the closure are handled separately
// by collectClosureLocals.)
func collectEnclosingVars(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	addFieldNames := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, fld := range fl.List {
			for _, n := range fld.Names {
				out[n.Name] = true
			}
		}
	}
	addFieldNames(fd.Recv)
	if fd.Type != nil {
		addFieldNames(fd.Type.Params)
		addFieldNames(fd.Type.Results)
	}
	// Body-level declarations OUTSIDE any FuncLit (so a g.Go closure's own
	// locals are excluded). ast.Inspect can't track exit, so we DON'T
	// descend into FuncLit subtrees: returning false from the visitor on a
	// FuncLit prunes that whole subtree, leaving only enclosing-scope decls.
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false // prune — closure-local decls are not enclosing vars
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							out[n.Name] = true
						}
					}
				}
			}
		}
		return true
	})
	return out
}

// collectClosureLocals returns identifiers declared INSIDE the FuncLit
// body (`:=` defines + `var` decls + params). These shadow the enclosing
// scope, so assigning to them is not a free-variable write.
func collectClosureLocals(lit *ast.FuncLit) map[string]bool {
	out := map[string]bool{}
	if lit.Type != nil && lit.Type.Params != nil {
		for _, fld := range lit.Type.Params.List {
			for _, n := range fld.Names {
				out[n.Name] = true
			}
		}
	}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, nm := range vs.Names {
							out[nm.Name] = true
						}
					}
				}
			}
		}
		return true
	})
	return out
}

// closureUsesMutexOrAtomic reports whether the closure body references a
// sync.Mutex lock or a sync/atomic operation — the legitimate guard for a
// shared write.
func closureUsesMutexOrAtomic(lit *ast.FuncLit) bool {
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Lock", "Unlock", "RLock", "RUnlock":
			found = true
		}
		// atomic.AddX / atomic.StoreX / *atomic.Value.Store, etc.
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "atomic" {
			found = true
		}
		return true
	})
	return found
}

// assignTargetBase returns the base variable name being assigned, plus
// whether the assignment is an INDEX write with a CONSTANT index (the
// disjoint-cell fan-in pattern). Handles:
//
//	x        → ("x", false)
//	x.field  → ("x", false)
//	x[const] → ("x", true)   -- safe: distinct constant cells
//	x[i]     → ("x", false)  -- non-constant index: treat as shared write
//	_        → ("", false)
func assignTargetBase(lhs ast.Expr) (string, bool) {
	switch e := lhs.(type) {
	case *ast.Ident:
		if e.Name == "_" {
			return "", false
		}
		return e.Name, false
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name, false
		}
		return "", false
	case *ast.IndexExpr:
		base := ""
		if id, ok := e.X.(*ast.Ident); ok {
			base = id.Name
		} else if sel, ok := e.X.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				base = id.Name
			}
		}
		if base == "" {
			return "", false
		}
		// Constant integer index → disjoint cell → safe.
		if lit, ok := e.Index.(*ast.BasicLit); ok && lit.Kind == token.INT {
			return base, true
		}
		return base, false
	case *ast.StarExpr:
		return assignTargetBase(e.X)
	}
	return "", false
}
