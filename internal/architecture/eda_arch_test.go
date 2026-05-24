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
		"github.com/leadkart/leadkart-go/internal/identity/app/actclaim":      true,
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
//   (a) wrap the handler with `messaging.IdempotencyMiddleware`, OR
//   (b) the handler body performs a natural-key precheck — calls a
//       repository `Get*` (or `Find*`) method that returns ErrNotFound
//       before the create path.
//
// Per Watermill / Brandur outbox-handler best practice: at-least-once
// delivery means duplicate dispatch is the rule. Every subscriber
// must be replay-safe.
//
// Detection is a heuristic — we accept either the explicit middleware
// wrap or any call expression named `Get*`/`Find*` inside the file.
// False negatives are possible; a file allow-list opts the subscriber
// out with a documented rationale.
func TestArch_SubscribersAreIdempotent(t *testing.T) {
	t.Parallel()

	// Files that are inherently idempotent without explicit wrap or
	// repo precheck (each with documented rationale in the file godoc).
	allowList := []string{
		"internal/identity/ports/subscribers/reuse_detected_siem.go", // append-only audit log; duplicate-safe
		"internal/identity/ports/subscribers/invalidate_cache.go",    // cache delete is idempotent by definition
		"internal/identity/ports/subscribers/registration.go",        // router-config helper; idempotency middleware wired at router level
		"internal/identity/ports/subscribers/revoke_families.go",     // Family.Revoke is no-op on already-revoked families (godoc cited)
		"internal/identity/ports/subscribers/email_sender.go",        // email provider enforces dedup at gateway; ADR 0057
	}

	getRE := regexp.MustCompile(`\b(Get|Find|Lookup|Exists)[A-Z]\w*\(`)
	idempMiddlewareRE := regexp.MustCompile(`\bIdempotency(Middleware|Decorator|Wrapper)\b`)

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
			for _, allowed := range allowList {
				if strings.HasSuffix(slashPath, allowed) {
					return
				}
			}
			text := string(src)
			if idempMiddlewareRE.MatchString(text) {
				return
			}
			if getRE.MatchString(text) {
				return
			}
			violations = append(violations, violation{file: path})
		})
	}

	if len(violations) > 0 {
		t.Logf("SUBSCRIBER IDEMPOTENCY VIOLATIONS — %d", len(violations))
		t.Logf("Watermill at-least-once delivery means every subscriber may")
		t.Logf("see the same event twice. Either wrap with")
		t.Logf("messaging.IdempotencyMiddleware OR perform a natural-key")
		t.Logf("precheck (Get*/Find* returning ErrNotFound before create).")
		for _, v := range violations {
			t.Errorf("%s — no IdempotencyMiddleware wrap + no Get/Find precheck", v.file)
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
func TestArch_IntegrationEventsHaveTopicMethod(t *testing.T) {
	t.Parallel()

	eventNameRE := regexp.MustCompile(`^[A-Z]\w*V\d+$`)
	// Topic() may live on the type directly OR on an embedded marker
	// struct (e.g. platformMarker / tenantMarker). We accept either by
	// also recognising compile-time `var _ TopicProvider = X{}` or by
	// detecting embedded type names that we know declare Topic().
	embeddedTopicProviders := map[string]bool{
		"platformMarker":      true,
		"tenantScopedMarker":  true,
		"tenantMarker":        true,
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
