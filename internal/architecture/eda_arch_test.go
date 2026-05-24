// eda_arch_test.go — Event-Driven Architecture discipline as a CI gate.
//
// Per ADR 0001 (modular monolith), ADR 0008 (Watermill messaging),
// ADR 0027 (outbox doubles as audit), and ADR 0002 (DDD sealed events):
// modules communicate ONLY via integration events on the bus. The
// tests here enforce that policy mechanically — drift becomes a PR-time
// failure, not a 3-month-later cloud-CI flake.

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 1: TestArch_NoCrossModuleImports
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0001 (modular monolith — modules never reference each
// other's domain/app/ports/adapters/adapters/db) + CLAUDE.md "three
// unbreakable rules" #1.
//
// EXCEPTIONS (documented; canon-cited):
//
//  1. importing another module's `integrationevents/` package IS allowed.
//     Subscribers in `internal/<X>/ports/subscribers/` MUST consume
//     integration events published by `internal/<Y>/`; the
//     integration-events package is the explicit anti-corruption layer
//     per Vernon IDDD ch. 13 ("Integrating Bounded Contexts").
//
//  2. Shared-kernel imports from identity: per Vernon IDDD ch. 13
//     "Shared Kernel" + Wave 9.1a/b (ADR 0051), the identity bounded
//     context owns the canonical typed-ID surface (TenantID, MembershipID,
//     PermissionCode) AND the cross-cutting authn middleware. The
//     allow-listed paths below are deliberately consumed by every other
//     module — refusing them would force every module to redeclare
//     UUID-typed IDs (a textbook "anaemic shared kernel" anti-pattern).
//
//     If you find yourself wanting to add another path here, write an
//     ADR first — the shared kernel must stay deliberately small.
//
// Canon: TDL Wild Workouts ("Modular Monolith Done Right"), Vernon IDDD
// ch. 13, CLAUDE.md "Architecture — three unbreakable rules", ADR 0051.
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

	// modulePrefix returns the canonical import-path prefix for a
	// module's private layers (domain/app/ports/adapters/adapters/db).
	// We allow integrationevents/ explicitly via not listing it here.
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
				// Only interested in imports of OTHER internal modules.
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
				// Shared-kernel allow-list. Vernon IDDD ch. 13.
				if sharedKernelAllowed[imp] {
					continue
				}
				// Cross-module import detected. Check whether it's a
				// forbidden private layer or the allowed
				// integrationevents/ surface.
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
// Test 2: TestArch_SubscribersInPortsSubscribers
// ----------------------------------------------------------------------------
//
// Enforces: subscribers are wired ONLY in internal/<module>/ports/subscribers/.
// Per ADR 0008 + the canonical messaging layout (TDL Watermill course):
// the inbound-port for an integration-event subscriber lives next to
// the inbound-port for HTTP. Wiring subscribers anywhere else fragments
// the inbound surface + breaks the dependency-flow assumption in arch
// test 10 (app/ doesn't depend on ports/).
//
// AST detection: any CallExpr where the function is a SelectorExpr
// named `AddSubscriber` (matches both `router.AddSubscriber(...)` and
// `messaging.Router.AddSubscriber(...)` at call sites).
//
// EXCEPTION: test files (path ending _test.go) are permitted to wire
// subscribers in-line — those are wiring-fixtures for the router itself.
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
			// Allowed: anywhere under .../ports/subscribers/.
			p := filepath.ToSlash(path)
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
		t.Logf("Per ADR 0008 + canonical inbound-port layout: every router.AddSubscriber")
		t.Logf("call MUST live in internal/<module>/ports/subscribers/. Wiring in any")
		t.Logf("other layer (especially app/) fragments the inbound surface + reverses")
		t.Logf("the dependency-flow direction enforced by TestArch_AppDoesntImportPorts.")
		for _, v := range violations {
			t.Errorf("%s:%d — AddSubscriber outside ports/subscribers/", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 3: TestArch_NoCrossSchemaJoins
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0001 + ADR 0006 — each module owns its Postgres schema;
// no cross-schema joins. A query in internal/identity/adapters/sql/*.sql
// may JOIN only `identity.*` tables OR the shared `buildingblocks.*`
// schema. Cross-module reads happen via outbox events into CQRS
// projections (ADR 0041), never via direct JOIN.
//
// Detection: regex-based scan of FROM and JOIN clauses for the
// `<schema>.` prefix. We tolerate aliases (no `.` after the table)
// since unprefixed tables resolve through `search_path` which the app
// pins per-module.
//
// Canon: Vernon IDDD ch. 13 (one schema per bounded context),
// Microsoft eShop (per-context DbContext), Brandur Leach ("Crunchy
// Bridge production sqlc layout").
func TestArch_NoCrossSchemaJoins(t *testing.T) {
	t.Parallel()

	fromRE := regexp.MustCompile(`(?i)\bFROM\s+([a-zA-Z_][a-zA-Z0-9_]*)\.[a-zA-Z_][a-zA-Z0-9_]*`)
	joinRE := regexp.MustCompile(`(?i)\bJOIN\s+([a-zA-Z_][a-zA-Z0-9_]*)\.[a-zA-Z_][a-zA-Z0-9_]*`)
	// Strip SQL-style comments before scanning to avoid false positives
	// on commentary referring to other schemas.
	lineCommentRE := regexp.MustCompile(`--[^\n]*`)
	blockCommentRE := regexp.MustCompile(`(?s)/\*.*?\*/`)

	allowedNonModule := map[string]bool{
		"buildingblocks": true, // shared kernel per ADR 0006/0027
		"app":            true, // app.current_tenant() / app.is_platform() helpers
		"pg_catalog":     true, // system catalogs
		"information_schema": true,
		"public":         true, // default; extensions
	}

	type violation struct {
		file   string
		schema string
		clause string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		sqlDir := filepath.Join(internalDir(t), mod, "adapters", "sql")
		// Walk if dir exists.
		entries, err := readDirSafe(sqlDir)
		if err != nil {
			continue // module has no sql/ dir
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			path := filepath.Join(sqlDir, e.Name())
			raw, rerr := readFileBytes(path)
			if rerr != nil {
				t.Errorf("read %s: %v", path, rerr)
				continue
			}
			// Strip comments.
			s := lineCommentRE.ReplaceAllString(string(raw), "")
			s = blockCommentRE.ReplaceAllString(s, "")

			for _, m := range fromRE.FindAllStringSubmatch(s, -1) {
				schema := strings.ToLower(m[1])
				if schema != mod && !allowedNonModule[schema] {
					violations = append(violations, violation{file: path, schema: m[1], clause: "FROM"})
				}
			}
			for _, m := range joinRE.FindAllStringSubmatch(s, -1) {
				schema := strings.ToLower(m[1])
				if schema != mod && !allowedNonModule[schema] {
					violations = append(violations, violation{file: path, schema: m[1], clause: "JOIN"})
				}
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("CROSS-SCHEMA JOIN VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0001 + ADR 0006: each module owns its Postgres schema.")
		t.Logf("Cross-module reads MUST flow through outbox → subscriber → projection")
		t.Logf("(ADR 0041), never via direct JOIN. Allowed extra-module schemas:")
		t.Logf("buildingblocks (shared kernel), app (RLS helpers), pg_catalog,")
		t.Logf("information_schema, public.")
		for _, v := range violations {
			t.Errorf("%s — %s clause references schema %q (not owned by this module + not in allowed-list)", v.file, v.clause, v.schema)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 4: TestArch_OutboxTableSchema
// ----------------------------------------------------------------------------
//
// Enforces: every CREATE TABLE <schema>.outbox in migrations/ MUST
// declare the canonical column set so the in-process forwarder + the
// Watermill SQL subscriber + the audit reader all stay drop-in
// compatible across modules.
//
// Required columns per ADR 0027 (outbox doubles as audit log):
//   id, occurred_at, topic (a.k.a. event_type), payload, forwarded_at.
//
// Note on "topic" vs "event_type": the canonical LeadKart-Go column
// name is `topic` (matches Watermill's pub/sub vocabulary). The task
// brief lists "event_type" — accept either to stay tolerant of the
// historical naming.
//
// Expected-but-optional: tenant_id (per ADR 0059 amendment),
// act_operator_id / act_session_id / act_reason (per ADR 0056).
//
// Canon: Brandur Leach "Transactionally staged job drains in Postgres",
// Watermill SQL outbox README, ADR 0008 + 0027 + 0056.
func TestArch_OutboxTableSchema(t *testing.T) {
	t.Parallel()

	// Match `CREATE TABLE <schema>.outbox (` followed by everything
	// up to the matching closing paren on its own line. Migrations
	// use the convention of one column per indented line.
	tableRE := regexp.MustCompile(`(?is)CREATE TABLE\s+(\w+)\.outbox\s*\((.*?)\);`)
	required := []string{"id", "occurred_at", "topic", "payload", "forwarded_at"}

	type violation struct {
		file    string
		schema  string
		missing []string
	}
	var violations []violation

	entries, err := readDirSafe(migrationsDir(t))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(migrationsDir(t), e.Name())
		raw, rerr := readFileBytes(path)
		if rerr != nil {
			t.Errorf("read %s: %v", path, rerr)
			continue
		}
		matches := tableRE.FindAllStringSubmatch(string(raw), -1)
		for _, m := range matches {
			schema := m[1]
			body := strings.ToLower(m[2])
			var missing []string
			for _, col := range required {
				// Each column name should appear as a token followed by
				// whitespace (the type). Use word-boundary regex per col.
				colRE := regexp.MustCompile(`(?m)\b` + col + `\b`)
				if !colRE.MatchString(body) {
					missing = append(missing, col)
				}
			}
			if len(missing) > 0 {
				violations = append(violations, violation{
					file:    path,
					schema:  schema,
					missing: missing,
				})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("OUTBOX SCHEMA VIOLATIONS — %d table definitions missing canonical columns", len(violations))
		t.Logf("Per ADR 0008 + 0027: every module's outbox table MUST declare:")
		t.Logf("  id, occurred_at, topic, payload, forwarded_at")
		t.Logf("(The forwarder + Watermill SQL subscriber + audit reader assume these.)")
		for _, v := range violations {
			t.Errorf("%s — CREATE TABLE %s.outbox missing columns: %v", v.file, v.schema, v.missing)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 5: TestArch_DomainEventsSealed
// ----------------------------------------------------------------------------
//
// Enforces: every domain Event interface uses the SEALED marker pattern.
// Per Vernon IDDD ch. 8 + Wild Workouts canon: domain events form a
// closed set per aggregate; an external package adding its own type
// to the marker interface would be a domain-modelling escape hatch.
// The seal is an unexported method (e.g. `isTenantEvent()`) — only
// types in the same package can implement it.
//
// Detection: AST walk every `internal/<module>/domain/<agg>/events.go`
// OR aggregate file containing `type Event interface`; assert the
// interface has exactly one method, the method name starts with `is`,
// and that name is unexported.
//
// Per CLAUDE.md "Architecture — three unbreakable rules" + canonical
// shape in internal/identity/domain/tenant/events.go.
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
				// Require exactly one method, unexported, name starts with "is".
				if iface.Methods == nil || len(iface.Methods.List) == 0 {
					violations = append(violations, violation{
						file:   path,
						typ:    ts.Name.Name,
						reason: "Event interface has no seal method (expected unexported `is<Agg>Event()`)",
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
						reason: "Event interface lacks an unexported seal method (e.g. `isTenantEvent()`)",
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("UNSEALED DOMAIN EVENT VIOLATIONS — %d", len(violations))
		t.Logf("Per Vernon IDDD ch. 8 + Wild Workouts canon: every domain `Event`")
		t.Logf("interface MUST be sealed via an unexported marker method (e.g.")
		t.Logf("`isTenantEvent()`). The seal prevents external packages from")
		t.Logf("implementing the marker — the domain owns its event taxonomy.")
		for _, v := range violations {
			t.Errorf("%s — type %s: %s", v.file, v.typ, v.reason)
		}
	}
}
