// eda_arch_test.go — Principle 4: Event-Driven Communication.
//
// Per ADR 0001 (modular monolith), ADR 0008 (Watermill messaging),
// ADR 0027 (outbox doubles as audit), and ADR 0051 (per-module
// integrationevents/ as the anti-corruption layer): modules communicate
// ONLY via integration events on the bus. The tests here enforce that
// policy mechanically — drift becomes a PR-time failure, not a
// 3-month-later cloud-CI flake.
//
// Tests in this file:
//   24. TestArch_NoCrossModuleImports
//   25. TestArch_SubscribersInPortsSubscribers
//   27. TestArch_SubscribersAreIdempotent
//   28. TestArch_IntegrationEventsHaveTopicMethod
//   29. TestArch_IntegrationEventsAreTenantScopedOrPlatform
//   30. TestArch_AppPublishesViaOutboxNotBus
//
// (Tests 26 = OutboxTableSchema and the original "cross-schema joins"
//  reside in db_schema_arch_test.go per Principle 11.)

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 24: TestArch_NoCrossModuleImports
// ----------------------------------------------------------------------------
//
// Modules NEVER reference each other's domain/app/ports/adapters.
// The integrationevents/ package is the explicit anti-corruption layer
// per Vernon IDDD ch. 13 + the canonical shared-kernel allow-list.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoCrossModuleImports(t *testing.T) {
	t.Parallel()

	mods := modulesUnderInternal(t)
	if len(mods) == 0 {
		t.Fatal("no modules discovered under internal/ — repo layout drift?")
	}

	// sharedKernelAllowed lists the exact identity import paths every
	// module may import. The list is deliberately closed — adding a
	// new entry requires an ADR amendment.
	sharedKernelAllowed := map[string]bool{
		"github.com/leadkart/leadkart-go/internal/identity/domain/tenant":     true,
		"github.com/leadkart/leadkart-go/internal/identity/domain/membership": true,
		"github.com/leadkart/leadkart-go/internal/identity/domain/permission": true,
		"github.com/leadkart/leadkart-go/internal/identity/ports/authn":       true,
		"github.com/leadkart/leadkart-go/internal/common/actclaim":            true,
	}

	forbiddenLayers := []string{"domain", "app", "ports", "adapters", "adapters/db"}

	type violation struct {
		file string
		imp  string
		from string
		to   string
	}
	var violations []violation

	for _, mod := range mods {
		modPath := filepath.Join(internalDir(t), mod)
		walkGoFiles(t, modPath, false, func(path string, src []byte) {
			imports := parseImports(t, path, src)
			for _, imp := range imports {
				const prefix = "github.com/leadkart/leadkart-go/internal/"
				if !strings.HasPrefix(imp, prefix) {
					continue
				}
				rest := strings.TrimPrefix(imp, prefix)
				parts := strings.SplitN(rest, "/", 2)
				if len(parts) < 2 {
					continue
				}
				targetMod := parts[0]
				targetRest := parts[1]
				if targetMod == mod || targetMod == "common" || targetMod == "architecture" {
					continue
				}
				if sharedKernelAllowed[imp] {
					continue
				}
				for _, layer := range forbiddenLayers {
					if targetRest == layer || strings.HasPrefix(targetRest, layer+"/") {
						violations = append(violations, violation{
							file: path,
							imp:  imp,
							from: mod,
							to:   targetMod + "/" + targetRest,
						})
						break
					}
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("CROSS-MODULE IMPORT VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0001 + CLAUDE.md: modules talk to each other ONLY via")
		t.Logf("integration events. The exception is internal/<X>/integrationevents/")
		t.Logf("which is the anti-corruption layer (Vernon IDDD ch. 13).")
		for _, v := range violations {
			t.Errorf("%s\n  imports private layer of another module: %s\n  (%s → %s)", v.file, v.imp, v.from, v.to)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 25: TestArch_SubscribersInPortsSubscribers
// ----------------------------------------------------------------------------
//
// Subscribers are wired ONLY in internal/<module>/ports/subscribers/.
// Per ADR 0008 + the canonical messaging layout (TDL Watermill course):
// the inbound-port for an integration-event subscriber lives next to
// the inbound-port for HTTP.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_SubscribersInPortsSubscribers(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "AddSubscriber" {
				return true
			}
			p := pathToSlash(path)
			if strings.Contains(p, "/ports/subscribers/") {
				return true
			}
			violations = append(violations, violation{
				file: path,
				line: fset.Position(call.Pos()).Line,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("SUBSCRIBER WIRING VIOLATIONS — %d call sites outside ports/subscribers/", len(violations))
		t.Logf("Per ADR 0008: every router.AddSubscriber call MUST live in")
		t.Logf("internal/<module>/ports/subscribers/.")
		for _, v := range violations {
			t.Errorf("%s:%d — AddSubscriber outside ports/subscribers/", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 27: TestArch_SubscribersAreIdempotent
// ----------------------------------------------------------------------------
//
// Every subscriber file in internal/<mod>/ports/subscribers/ must
// provide an idempotency mechanism — either:
//
//	(a) wrap the handler with `messaging.IdempotencyMiddleware`, OR
//	(b) the handler body performs a natural-key precheck — calls a
//	    repository `Get*` (or `Find*`) method that returns ErrNotFound
//	    before the create path.
//
// Per Watermill / Brandur outbox-handler best practice: at-least-once
// delivery means duplicate dispatch is the rule. Every subscriber
// must be replay-safe.
//
// Detection accepts ANY of three signals (in order — first match wins):
//
//	(1) explicit `messaging.IdempotencyMiddleware` / Decorator / Wrapper
//	    reference (the universal infra-level guarantee);
//	(2) a `Get*`/`Find*`/`Lookup*`/`Exists*` call inside the file
//	    (inline natural-key precheck);
//	(3) an inline marker comment
//	    `// arch-test:idempotency-via-<mechanism> — <reason>`
//	    — for subscribers whose dedup happens one call-frame down
//	    (the precheck lives in the command they delegate to), in an
//	    infra layer that wraps the whole router, or in the wire layer
//	    (DTO files with no handler logic).
//
// The inline marker is the canon escape valve for the (legitimate)
// case where the heuristic can't see the precheck because the dedup
// lives in a sibling file. Each marker must end `— <reason>` so the
// rationale travels with the code, not with a registry list.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_SubscribersAreIdempotent(t *testing.T) {
	t.Parallel()

	getRE := regexp.MustCompile(`\b(Get|Find|Lookup|Exists)[A-Z]\w*\(`)
	idempMiddlewareRE := regexp.MustCompile(`\bIdempotency(Middleware|Decorator|Wrapper)\b`)
	// Marker must include a trailing reason after an em-dash so the
	// rationale lives with the code. Matches:
	//   // arch-test:idempotency-via-natural-key-precheck — reason
	//   // arch-test:idempotency-via-router-middleware — reason
	//   // arch-test:idempotency-via-wire-shape-only — reason
	//   // arch-test:idempotency-via-append-only-log — reason
	//   // arch-test:idempotency-via-gateway-dedup — reason
	//   // arch-test:idempotency-via-noop-on-replay — reason
	inlineMarkerRE := regexp.MustCompile(`//\s*arch-test:idempotency-via-[a-z][a-z0-9-]*\s+—\s+\S`)

	type violation struct {
		file string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		subDir := filepath.Join(internalDir(t), mod, "ports", "subscribers")
		walkGoFiles(t, subDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			if strings.HasSuffix(slashPath, "/doc.go") {
				return
			}
			text := string(src)
			if idempMiddlewareRE.MatchString(text) {
				return
			}
			if getRE.MatchString(text) {
				return
			}
			if inlineMarkerRE.MatchString(text) {
				return
			}
			violations = append(violations, violation{file: path})
		})
	}

	if len(violations) > 0 {
		t.Logf("SUBSCRIBER IDEMPOTENCY VIOLATIONS — %d", len(violations))
		t.Logf("Watermill at-least-once delivery means every subscriber may")
		t.Logf("see the same event twice. Pick one of:")
		t.Logf("  (1) wrap with messaging.IdempotencyMiddleware / Decorator,")
		t.Logf("  (2) natural-key precheck (Get*/Find*/Lookup*/Exists* returning ErrNotFound before create),")
		t.Logf("  (3) inline marker `// arch-test:idempotency-via-<mechanism> — <reason>` when the dedup is one call-frame down.")
		for _, v := range violations {
			t.Errorf("%s — no IdempotencyMiddleware wrap + no Get/Find precheck + no inline arch-test marker", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 28: TestArch_IntegrationEventsHaveTopicMethod
// ----------------------------------------------------------------------------
//
// Every concrete event struct under internal/<mod>/integrationevents/
// (file ending V<N>.go or matching the canonical event naming) must
// expose a `Topic()` method returning a string — the canonical wire
// alias under the registry. Without Topic() the event cannot be
// registered + routed.
//
// Detection: per module, parse every non-test .go file in
// integrationevents/; collect every exported struct type whose name
// ends in `V<digit>+`; assert a method declaration on (T or *T) named
// `Topic` exists.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_IntegrationEventsHaveTopicMethod(t *testing.T) {
	t.Parallel()

	eventNameRE := regexp.MustCompile(`^[A-Z]\w*V\d+$`)
	// Topic() may live on the type directly OR on an embedded marker
	// struct (e.g. platformMarker / tenantMarker). We accept either by
	// also recognising compile-time `var _ TopicProvider = X{}` or by
	// detecting embedded type names that we know declare Topic().
	embeddedTopicProviders := map[string]bool{
		"platformMarker":     true,
		"tenantScopedMarker": true,
		"tenantMarker":       true,
	}

	type violation struct {
		file string
		typ  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		ieDir := filepath.Join(internalDir(t), mod, "integrationevents")
		types := map[string]ast.Node{} // typeName -> the *ast.StructType for embed inspection
		typesFiles := map[string]string{}
		hasTopic := map[string]bool{}
		typeEmbeds := map[string][]string{}

		walkGoFiles(t, ieDir, false, func(path string, src []byte) {
			_, f := parseFile(t, path, src)
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok {
							continue
						}
						if !eventNameRE.MatchString(ts.Name.Name) {
							continue
						}
						types[ts.Name.Name] = st
						typesFiles[ts.Name.Name] = path
						// Collect embedded type names.
						for _, field := range st.Fields.List {
							if len(field.Names) == 0 { // embedded
								if id, ok := field.Type.(*ast.Ident); ok {
									typeEmbeds[ts.Name.Name] = append(typeEmbeds[ts.Name.Name], id.Name)
								}
							}
						}
					}
				case *ast.FuncDecl:
					if d.Name.Name != "Topic" || d.Recv == nil || len(d.Recv.List) == 0 {
						continue
					}
					rt := d.Recv.List[0].Type
					if star, ok := rt.(*ast.StarExpr); ok {
						rt = star.X
					}
					if id, ok := rt.(*ast.Ident); ok {
						hasTopic[id.Name] = true
					}
				}
			}
		})

		for typ := range types {
			if hasTopic[typ] {
				continue
			}
			// Embedded marker that provides Topic() is acceptable.
			satisfied := false
			for _, emb := range typeEmbeds[typ] {
				if embeddedTopicProviders[emb] || hasTopic[emb] {
					satisfied = true
					break
				}
			}
			if !satisfied {
				violations = append(violations, violation{file: typesFiles[typ], typ: typ})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("INTEGRATION-EVENT Topic() VIOLATIONS — %d", len(violations))
		t.Logf("Every <Name>V<N> struct must declare (or inherit via an")
		t.Logf("embedded marker) `Topic() string`. Without Topic() the registry")
		t.Logf("cannot route the event.")
		for _, v := range violations {
			t.Errorf("%s — type %s missing Topic() method (direct or via embed)", v.file, v.typ)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 29: TestArch_IntegrationEventsAreTenantScopedOrPlatform
// ----------------------------------------------------------------------------
//
// Every concrete event in internal/<mod>/integrationevents/ must
// implement EXACTLY ONE of the marker interfaces TenantScoped or
// Platform. The marker drives routing + audit-log enrichment.
//
// Per per-module integrationevents/arch_test.go (cross-promoted): the
// rule has been per-module; this fitness function ensures NEW modules
// (CRM, orders, dispatch, etc.) carry the same discipline by walking
// them all from the architecture/ package.
//
// Detection: AST-walk every event struct; require a method named
// `IsTenantScoped` or `IsPlatform` to exist on the type. The per-
// module suite asserts EXCLUSIVE OR via the interface marker; this
// cross-cutting test asserts at-least-one (drift floor).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_IntegrationEventsAreTenantScopedOrPlatform(t *testing.T) {
	t.Parallel()

	eventNameRE := regexp.MustCompile(`^[A-Z]\w*V\d+$`)
	// Compile-time assertion regex matches both shapes:
	//   var _ Platform     = TenantRegisteredV1{}
	//   var _ TenantScoped = MembershipCreatedV1{}
	assertionRE := regexp.MustCompile(`\b_\s+(TenantScoped|Platform)\s*=\s*(\*?)([A-Z]\w*V\d+)\b`)

	type violation struct {
		file string
		typ  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		ieDir := filepath.Join(internalDir(t), mod, "integrationevents")
		types := map[string]bool{}
		typesFiles := map[string]string{}
		hasMarker := map[string]bool{}

		walkGoFiles(t, ieDir, false, func(path string, src []byte) {
			_, f := parseFile(t, path, src)
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
					if _, ok := ts.Type.(*ast.StructType); !ok {
						continue
					}
					if eventNameRE.MatchString(ts.Name.Name) {
						types[ts.Name.Name] = true
						typesFiles[ts.Name.Name] = path
					}
				}
			}
			// Scan textually for compile-time interface assertions.
			for _, m := range assertionRE.FindAllStringSubmatch(string(src), -1) {
				hasMarker[m[3]] = true
			}
		})

		for typ := range types {
			if !hasMarker[typ] {
				violations = append(violations, violation{file: typesFiles[typ], typ: typ})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("INTEGRATION-EVENT SCOPE-MARKER VIOLATIONS — %d", len(violations))
		t.Logf("Every event must declare a compile-time interface assertion:")
		t.Logf("  var _ TenantScoped = <Event>V<N>{}  // or")
		t.Logf("  var _ Platform     = <Event>V<N>{}")
		t.Logf("The marker drives routing + audit-log enrichment.")
		for _, v := range violations {
			t.Errorf("%s — type %s missing var-assert against TenantScoped or Platform", v.file, v.typ)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 30: TestArch_AppPublishesViaOutboxNotBus
// ----------------------------------------------------------------------------
//
// Per ADR 0008: integration events are written to the per-module
// outbox table inside the same tx as the aggregate mutation; a
// separate forwarder polls the outbox and publishes to the bus.
// App-layer code MUST NOT call `Publisher.Publish` or
// `MessagePublisher.Publish` directly — that bypasses the outbox and
// breaks the at-least-once + ordered-by-aggregate guarantees.
//
// Detection: AST-walk app/ files; flag CallExprs with selector name
// `Publish` whose receiver is a watermill-typed publisher.
//
// Heuristic — we accept any `*.Publish(...)` call inside app/ as a
// violation unless allow-listed. False positive: a domain repo named
// `OutboxPublisher` may legitimately expose Publish at the app layer
// when going through the outbox helper.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AppPublishesViaOutboxNotBus(t *testing.T) {
	t.Parallel()

	// Allow-list: app/-layer Publish callers that go through the outbox
	// indirection (the Outbox itself uses .Publish on a wrapped pub).
	allowList := []string{
		// Currently empty — the outbox forwarder lives in adapters/.
	}

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		appDir := filepath.Join(internalDir(t), mod, "app")
		walkGoFiles(t, appDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			for _, allowed := range allowList {
				if strings.HasSuffix(slashPath, allowed) {
					return
				}
			}
			imports := parseImports(t, path, src)
			usesWatermill := false
			for _, imp := range imports {
				if strings.HasPrefix(imp, "github.com/ThreeDotsLabs/watermill") {
					usesWatermill = true
					break
				}
			}
			if !usesWatermill {
				return
			}
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := callName(call.Fun)
				if name == "Publish" {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("APP-LAYER DIRECT-PUBLISH VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0008: integration events go through the outbox, not")
		t.Logf("a direct Publisher.Publish call in app/. The outbox guarantees")
		t.Logf("at-least-once delivery + ordered drainage; direct publish loses")
		t.Logf("both.")
		for _, v := range violations {
			t.Errorf("%s:%d — .Publish() inside app/ (use the outbox via the repo Add path)", v.file, v.line)
		}
	}
}

// ============================================================================
// Principle C deep — Watermill / messaging discipline (5 tests added per the
// comprehensive catalog brief).
// ============================================================================

// ----------------------------------------------------------------------------
// C1: TestArch_TopicNamingConvention
// ----------------------------------------------------------------------------
//
// Every topic string declared via a Topic() method on an integration
// event must follow `<module>.<entity>_<action>.v<N>` (lowercase, no
// PascalCase, explicit version segment). Drift in the topic naming
// surface is a hard-to-roll-back change: subscribers index on the
// exact string. ADR 0008 + messaging.md doctrine.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_TopicNamingConvention(t *testing.T) {
	t.Parallel()

	returnStrRE := regexp.MustCompile(`return\s+"([^"]+)"`)
	topicFuncRE := regexp.MustCompile(`(?s)func\s*\([^)]+\)\s+Topic\(\)\s+string\s*\{([^}]+)\}`)
	canon := regexp.MustCompile(`^[a-z]+\.[a-z][a-z0-9_]*\.v\d+$`)

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "integrationevents")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			for _, m := range topicFuncRE.FindAllStringSubmatch(body, -1) {
				for _, sm := range returnStrRE.FindAllStringSubmatch(m[1], -1) {
					if !canon.MatchString(sm[1]) {
						bad = append(bad, pathToSlash(path)+": "+sm[1])
					}
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("topic string violates `<module>.<entity>_<action>.v<N>` convention (ADR 0008):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// C2: TestArch_HandlerNamesConstDefined
// ----------------------------------------------------------------------------
//
// `router.AddSubscriber(name, ...)` / `router.AddNoPublisherHandler(name, ...)`
// — the first arg is the handler dedup key. Inlining the string is a
// footgun: renaming silently re-processes every message because
// `<module>.processed_messages` indexes on the new name.
//
// Predicate: every call's first arg must be a Go identifier
// (`HandlerXxx`), not a string literal.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_HandlerNamesConstDefined(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	addCallRE := regexp.MustCompile(`router\.(?:AddNoPublisherHandler|AddSubscriber|AddHandler)\(\s*("[^"]*"|\w+)`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "ports", "subscribers")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			for _, m := range addCallRE.FindAllStringSubmatch(body, -1) {
				if strings.HasPrefix(m[1], `"`) {
					bad = append(bad, pathToSlash(path)+": "+m[1])
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("router.AddSubscriber uses inline string for handler name — use Handler* const (rename = silent reprocess):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// C3: TestArch_SubscribersDontPublish
// ----------------------------------------------------------------------------
//
// Subscriber handlers MUST NOT inline-publish a new event. Saga
// fanout is the outbox + a second subscriber on the parent event —
// inline re-publish skips the outbox + breaks at-least-once
// guarantees + opens up cycles. ThreeDotsLabs "Go Event-Driven"
// pattern.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_SubscribersDontPublish(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	pubCallRE := regexp.MustCompile(`\b(?:publisher|bus|pub|forwarder)\.Publish\(`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "ports", "subscribers")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			if pubCallRE.MatchString(body) {
				bad = append(bad, pathToSlash(path))
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("subscriber inline-publishes — saga fanout MUST go via outbox + sibling subscriber (TDL Go Event-Driven):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// C4: TestArch_OutboxForwarderUsesTxScopePlatform
// ----------------------------------------------------------------------------
//
// The outbox forwarder reads from `<module>.outbox` cross-tenant; it
// MUST run under `pg.TxScopePlatform` (RLS bypassed via schema-owner
// role) otherwise SET LOCAL app.tenant_id makes it see only NULL rows.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_OutboxForwarderUsesTxScopePlatform(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "adapters")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			base := strings.ToLower(filepath.Base(path))
			body := string(src)
			isOutbox := strings.Contains(base, "outbox_forwarder") ||
				strings.Contains(base, "outbox.go") ||
				(strings.Contains(base, "forwarder") && strings.Contains(body, "outbox"))
			if !isOutbox {
				return
			}
			if strings.Contains(body, "TxScopePlatform") {
				return
			}
			if strings.Contains(body, "arch-test:no-platform-scope") {
				return
			}
			bad = append(bad, pathToSlash(path))
		})
	}

	if len(bad) > 0 {
		t.Fatalf("outbox forwarder missing TxScopePlatform — RLS will hide cross-tenant rows:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// C5: TestArch_WatermillErrorReturnIsRetry
// ----------------------------------------------------------------------------
//
// Subscriber handler functions return error → Watermill retries.
// There's no "fatal, don't retry" return path. Each `return ... err`
// in a subscriber MUST be accompanied by a `// retry` or `// fatal`
// marker — forces the author's intent to be explicit so retry-storm
// regressions don't slip through review.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_WatermillErrorReturnIsRetry(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "ports", "subscribers")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			lines := strings.Split(string(src), "\n")
			inHandler := false
			braceDepth := 0
			for i, ln := range lines {
				if !inHandler {
					if strings.Contains(ln, "*message.Message") &&
						strings.Contains(ln, "error") {
						inHandler = true
						braceDepth = strings.Count(ln, "{") - strings.Count(ln, "}")
					}
					continue
				}
				braceDepth += strings.Count(ln, "{") - strings.Count(ln, "}")
				trimmed := strings.TrimSpace(ln)
				if strings.HasPrefix(trimmed, "return ") &&
					(strings.Contains(trimmed, "err") || strings.Contains(trimmed, "fmt.Errorf")) {
					if trimmed == "return nil" {
						continue
					}
					// Marker may sit on the same line OR in the
					// containing case/block godoc up to 5 lines above.
					hasMarker := strings.Contains(ln, "// retry") ||
						strings.Contains(ln, "// fatal")
					if !hasMarker {
						for j := i - 1; j >= 0 && j > i-6; j-- {
							if strings.Contains(lines[j], "// retry") ||
								strings.Contains(lines[j], "// fatal") {
								hasMarker = true
								break
							}
						}
					}
					if !hasMarker {
						bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
					}
				}
				if braceDepth <= 0 {
					inHandler = false
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("subscriber returns err without // retry or // fatal marker (Watermill retries on any non-nil err):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// helper: small int formatter (avoids strconv import where one symbol is needed).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789"
	var buf []byte
	for n > 0 {
		buf = append([]byte{digits[n%10]}, buf...)
		n /= 10
	}
	return string(buf)
}
