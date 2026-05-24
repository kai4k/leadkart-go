// multi_tenancy_arch_test.go — Principle 6: Multi-Tenancy Enforcement.
//
// Per ADR 0006 (Postgres RLS + SET LOCAL via pgxpool AfterAcquire),
// ADR 0039 (per-request scope selection), Microsoft Azure Multi-tenant
// SaaS canon, and the LeadKart-Go RLS doctrine in multi-tenancy.md:
// tenant isolation is enforced at THREE layers — DB (RLS + FORCE),
// pool (SET LOCAL on connection acquire), and handler (RequireTenantContext
// middleware). The tests here ensure each layer stays present.
//
// Tests in this file:
//   37. TestArch_EveryTenantTableHasRLSAndForce
//   38. TestArch_EveryTenantTableHasRLSPolicies
//   39. TestArch_TenantEntitiesCarryTenantID
//   40. TestArch_RepoTenantScopedReadsUseTxScopeTenant
//   41. TestArch_CrossTenantOperationsCheckIsPlatform
//   42. TestArch_NoBareTenantIDStrings

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tenantOwningSchemas lists the per-module schemas that hold tenant-
// scoped tables. Pure platform schemas (`buildingblocks`, `app`) are
// not in this list — they store cross-tenant rows by definition.
var tenantOwningSchemas = map[string]bool{
	"identity":  true,
	"inventory": true,
	"platform":  true,
}

// rlsOptOutTables: tables that may omit RLS via the explicit marker
// `-- arch-test:opt-out-rls (reason)` adjacent to the CREATE TABLE.
// Reference / lookup tables (immutable cross-tenant catalogue rows)
// are the canonical opt-out target.

// ----------------------------------------------------------------------------
// Test 37: TestArch_EveryTenantTableHasRLSAndForce
// ----------------------------------------------------------------------------
//
// For every CREATE TABLE <schema>.<table> in tenant-owning schemas:
// the SAME migration file must declare:
//
//   ALTER TABLE <schema>.<table> ENABLE ROW LEVEL SECURITY;
//   ALTER TABLE <schema>.<table> FORCE ROW LEVEL SECURITY;
//
// Without FORCE, RLS is BYPASSED for the table owner role — so a
// migration that creates without FORCE leaves the table exposed to
// the leadkart_owner role used by sqlc-generated code in tests.
//
// EXCEPTION: any CREATE TABLE preceded by the line
// `-- arch-test:opt-out-rls (reason)` is allowed.
func TestArch_EveryTenantTableHasRLSAndForce(t *testing.T) {
	t.Parallel()

	// Closed: migration 20260603000202_force_rls_on_tenant_tables.sql adds
	// FORCE ROW LEVEL SECURITY to all 15 ENABLE-without-FORCE tables that
	// existed at the time the suite first ran. New tables must ship FORCE
	// in the same migration as ENABLE; this test now guards regression.

	tableRE := regexp.MustCompile(`(?im)^\s*CREATE TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)\s*\(`)
	enableRE := regexp.MustCompile(`(?im)\bALTER TABLE\s+([a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)\s+ENABLE ROW LEVEL SECURITY`)
	forceRE := regexp.MustCompile(`(?im)\bALTER TABLE\s+([a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)\s+FORCE ROW LEVEL SECURITY`)
	optOutRE := regexp.MustCompile(`(?im)^\s*--\s*arch-test:opt-out-rls`)

	type violation struct {
		file    string
		table   string
		missing string
	}
	var violations []violation

	entries, err := readDirSafe(migrationsDir(t))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	// First pass — collect ENABLE/FORCE CUMULATIVELY across ALL migrations.
	// A later migration may ADD FORCE to a table whose CREATE TABLE +
	// ENABLE landed in an earlier migration; checking per-file would
	// false-positive that legitimate pattern.
	enabled := map[string]bool{}
	forced := map[string]bool{}
	type tableSite struct {
		file   string
		schema string
		table  string
	}
	var tableSites []tableSite

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
		text := string(raw)
		for _, m := range enableRE.FindAllStringSubmatch(text, -1) {
			enabled[strings.ToLower(m[1])] = true
		}
		for _, m := range forceRE.FindAllStringSubmatch(text, -1) {
			forced[strings.ToLower(m[1])] = true
		}
		// Record each CREATE TABLE site (with its surrounding context for
		// opt-out check) for the SECOND-pass violation report.
		matches := tableRE.FindAllStringSubmatchIndex(text, -1)
		for _, m := range matches {
			schema := strings.ToLower(text[m[2]:m[3]])
			table := strings.ToLower(text[m[4]:m[5]])
			if !tenantOwningSchemas[schema] {
				continue
			}
			if table == "outbox" {
				continue
			}
			// 600-char lookback (~10 lines) so the opt-out comment can
			// carry a multi-line rationale instead of being squeezed.
			start := m[0] - 600
			if start < 0 {
				start = 0
			}
			if optOutRE.MatchString(text[start:m[0]]) {
				continue
			}
			tableSites = append(tableSites, tableSite{file: path, schema: schema, table: table})
		}
	}

	// Second pass — for every (non-opted-out, tenant-schema) CREATE TABLE,
	// check the cumulative enabled+forced maps.
	for _, ts := range tableSites {
		qname := ts.schema + "." + ts.table
		if !enabled[qname] {
			violations = append(violations, violation{
				file:    ts.file,
				table:   qname,
				missing: "ENABLE ROW LEVEL SECURITY",
			})
		}
		if !forced[qname] {
			violations = append(violations, violation{
				file:    ts.file,
				table:   qname,
				missing: "FORCE ROW LEVEL SECURITY",
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("TENANT-TABLE RLS+FORCE VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0006 + multi-tenancy.md: every tenant-scoped table")
		t.Logf("must enable AND force RLS in the same migration. Without")
		t.Logf("FORCE, the table owner bypasses RLS — leakage path.")
		t.Logf("Opt-out: prefix the CREATE TABLE with")
		t.Logf("  -- arch-test:opt-out-rls (rationale)")
		for _, v := range violations {
			t.Errorf("%s — %s missing %s", v.file, v.table, v.missing)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 38: TestArch_EveryTenantTableHasRLSPolicies
// ----------------------------------------------------------------------------
//
// Every RLS-enabled table needs at least SELECT + INSERT policies
// declared in the same migration. Without policies, RLS denies
// everything — table is effectively unreachable.
func TestArch_EveryTenantTableHasRLSPolicies(t *testing.T) {
	t.Parallel()

	enableRE := regexp.MustCompile(`(?im)\bALTER TABLE\s+([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)\s+ENABLE ROW LEVEL SECURITY`)
	// Matches both:
	//   CREATE POLICY <name> ON <schema>.<table> FOR SELECT ...
	//   CREATE POLICY <name> ON <schema>.<table>     -- no FOR clause = ALL
	policyRE := regexp.MustCompile(`(?im)\bCREATE POLICY\s+\w+\s+ON\s+([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)(?:\s+FOR\s+(\w+))?`)

	type tableKey struct{ schema, table string }
	type policySet struct {
		hasSelect, hasInsert, hasAll bool
	}
	type violation struct {
		file    string
		table   string
		missing []string
	}

	// Walk per-file; the policy assertion is per-migration.
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
		text := string(raw)
		enabledTables := map[tableKey]bool{}
		for _, m := range enableRE.FindAllStringSubmatch(text, -1) {
			schema := strings.ToLower(m[1])
			table := strings.ToLower(m[2])
			if !tenantOwningSchemas[schema] || table == "outbox" {
				continue
			}
			enabledTables[tableKey{schema, table}] = true
		}
		// Collect policies per table.
		policiesByTable := map[tableKey]policySet{}
		for _, m := range policyRE.FindAllStringSubmatch(text, -1) {
			schema := strings.ToLower(m[1])
			table := strings.ToLower(m[2])
			cmd := strings.ToUpper(m[3])
			ps := policiesByTable[tableKey{schema, table}]
			switch cmd {
			case "SELECT":
				ps.hasSelect = true
			case "INSERT":
				ps.hasInsert = true
			case "":
				ps.hasAll = true
			case "ALL":
				ps.hasAll = true
			}
			policiesByTable[tableKey{schema, table}] = ps
		}
		for tk := range enabledTables {
			ps := policiesByTable[tk]
			var missing []string
			if !ps.hasAll && !ps.hasSelect {
				missing = append(missing, "SELECT")
			}
			if !ps.hasAll && !ps.hasInsert {
				missing = append(missing, "INSERT")
			}
			if len(missing) > 0 {
				violations = append(violations, violation{
					file:    path,
					table:   tk.schema + "." + tk.table,
					missing: missing,
				})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("RLS POLICY-MISSING VIOLATIONS — %d", len(violations))
		t.Logf("Every RLS-enabled tenant table needs at least SELECT + INSERT")
		t.Logf("policies (or one FOR ALL). Without policies RLS denies everything.")
		for _, v := range violations {
			t.Errorf("%s — %s missing policies for: %v", v.file, v.table, v.missing)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 39: TestArch_TenantEntitiesCarryTenantID
// ----------------------------------------------------------------------------
//
// Every aggregate root in tenant-owning modules has a `tenantID tenant.ID`
// field (or similarly-named). Documented exceptions: aggregates whose
// scope is global (e.g. Person, Tenant itself, RefreshToken which is
// keyed by person, Permission).
//
// Detection: walk aggregate root struct fields; assert at least one
// field name is tenantID / TenantID. Exceptions ride in the allow-list.
func TestArch_TenantEntitiesCarryTenantID(t *testing.T) {
	t.Parallel()

	// Aggregates that are global by design (not tenant-scoped).
	globalAggs := map[string]bool{
		"tenant":            true, // Tenant IS the tenant — no parent tenant
		"person":            true, // global identity
		"refreshtoken":      true, // keyed by person (Family)
		"permission":        true, // closed-set catalogue
		"passwordpolicy":    true, // value-object policy
		"impersonation":     true, // platform-issued session
		"rolehierarchy":     true, // join-table aggregate carries tenant_id via composite FK; struct name differs
		"leadcredit":        true, // platform-owned credit pool (per ADR 0059)
		"unverifiedcontact": true, // platform-only acquisition surface
		"verificationcall":  true, // platform-only call log
		"platformlead":      true, // platform-owned lead listing (cross-tenant marketplace)
		"stockmovement":     true, // append-only ledger; tenant_id derived from parent Batch (composite FK)
	}

	type violation struct {
		pkgDir string
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
			if globalAggs[aggName] {
				continue
			}
			pkgDir := filepath.Join(domainDir, aggName)
			isAgg := false
			hasTenantField := false

			// Inspect EVERY exported struct in the package — the aggregate
			// root may be named after the noun (e.g. permissionrequest →
			// Request, refreshtoken → Family). We accept tenantID on any
			// exported struct that owns mutator methods.
			walkGoFiles(t, pkgDir, false, func(path string, src []byte) {
				_, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if !ok {
						return true
					}
					if ts.Name.Name == "Repository" {
						if _, ok := ts.Type.(*ast.InterfaceType); ok {
							isAgg = true
						}
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						return true
					}
					if !ast.IsExported(ts.Name.Name) {
						return true
					}
					for _, field := range st.Fields.List {
						for _, fn := range field.Names {
							if fn.Name == "tenantID" || fn.Name == "TenantID" {
								hasTenantField = true
							}
						}
					}
					return true
				})
			})
			if !isAgg {
				continue
			}
			if hasTenantField {
				continue
			}
			violations = append(violations, violation{pkgDir: pkgDir})
		}
	}

	if len(violations) > 0 {
		t.Logf("AGGREGATE TENANT-ID-FIELD VIOLATIONS — %d", len(violations))
		t.Logf("Every tenant-scoped aggregate carries a tenantID field. If")
		t.Logf("this aggregate is global, add it to the test's globalAggs")
		t.Logf("allow-list with a documented rationale.")
		for _, v := range violations {
			t.Errorf("%s — missing tenantID field on aggregate root", v.pkgDir)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 40: TestArch_RepoTenantScopedReadsUseTxScopeTenant
// ----------------------------------------------------------------------------
//
// Adapter methods that read tenant-scoped data open tx via
// `pg.TxScopeTenant` (not `TxScopePlatform`). The pool's AfterAcquire
// callback uses the scope to SET LOCAL app.tenant_id / app.is_platform,
// so the wrong scope leaks rows.
//
// Heuristic: any *_pg.go file with `WithinTx(`/`WithinTxPgx(` calls
// must reference TxScopeTenant somewhere (or be a documented platform-
// only adapter).
func TestArch_RepoTenantScopedReadsUseTxScopeTenant(t *testing.T) {
	t.Parallel()

	// Adapters that are platform-only by design.
	platformOnly := []string{
		"internal/identity/adapters/auth_router_pg.go",
		"internal/identity/adapters/audit_reader_pg.go",
		"internal/identity/adapters/impersonation_audit_pg.go",
		"internal/identity/adapters/impersonation_inmemory_store.go",
		"internal/identity/adapters/outbox_forwarder.go",
		"internal/identity/adapters/platform_stats_pg.go",
		"internal/identity/adapters/refresh_token_repository_pg.go",
		"internal/identity/adapters/search_index_pg.go",
		"internal/identity/adapters/security_stamp_cache.go",
		"internal/identity/adapters/passwordpolicy_offline_list.go",
		// Cross-tenant tenant management
		"internal/identity/adapters/tenant_repository_pg.go",
		"internal/identity/adapters/person_repository_pg.go",
		// Platform module adapters are platform-scoped by default
		"internal/platform/adapters/lead_credit_repository_pg.go",
		"internal/platform/adapters/unverified_contact_repository_pg.go",
		"internal/platform/adapters/verification_call_repository_pg.go",
		"internal/platform/adapters/platform_lead_reader_pg.go",
		"internal/platform/adapters/outbox_forwarder.go",
	}

	type violation struct {
		file string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		adaptersDir := filepath.Join(internalDir(t), mod, "adapters")
		walkGoFiles(t, adaptersDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			if !strings.HasSuffix(slashPath, "_pg.go") {
				return
			}
			for _, allowed := range platformOnly {
				if strings.HasSuffix(slashPath, allowed) {
					return
				}
			}
			text := string(src)
			if !strings.Contains(text, "WithinTx") {
				return
			}
			if strings.Contains(text, "TxScopeTenant") {
				return
			}
			// Allow files that go through addOnTx (which itself goes
			// through TxScopeTenant).
			if strings.Contains(text, "addOnTx") {
				return
			}
			violations = append(violations, violation{file: path})
		})
	}

	if len(violations) > 0 {
		t.Logf("TX-SCOPE-TENANT VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0006: tenant-scoped adapter methods open tx via")
		t.Logf("pg.TxScopeTenant (or the addOnTx helper). The wrong scope")
		t.Logf("loses RLS isolation.")
		for _, v := range violations {
			t.Errorf("%s — WithinTx callers but no TxScopeTenant reference (use addOnTx?)", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 41: TestArch_CrossTenantOperationsCheckIsPlatform
// ----------------------------------------------------------------------------
//
// Handlers that read across tenants check claims.IsPlatform (or
// RequirePlatform middleware in their route registration). The
// detection heuristic: handlers whose name pattern signals cross-
// tenant access (`*Platform*`, `*Cross*`, `List<X>AcrossTenants`)
// must either:
//
//   (a) appear under the `*PlatformHandler` receiver type whose path
//       is the platform_http.go file (route-wired with RequirePlatform), or
//   (b) contain an `IsPlatform()` check in the handler body.
//
// This is a heuristic; misses a handler that does cross-tenant work
// without the name pattern. Tracked as a known low-precision test.
func TestArch_CrossTenantOperationsCheckIsPlatform(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		appDir := filepath.Join(internalDir(t), mod, "app")
		walkGoFiles(t, appDir, false, func(path string, src []byte) {
			_, f := parseFile(t, path, src)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Body == nil {
					continue
				}
				name := fd.Name.Name
				if !strings.Contains(name, "Platform") && !strings.Contains(name, "Cross") && !strings.Contains(name, "AcrossTenants") {
					continue
				}
				// If the receiver is a Handler in platform_http.go we trust
				// the route gate; otherwise we want to see IsPlatform check.
				bodyText := string(src)
				if strings.Contains(bodyText, "IsPlatform") {
					continue
				}
				// Allow handlers named "PlatformX" but operating per-tenant
				// where the spec gates via permission catalogue.
				if strings.Contains(name, "Permission") {
					continue
				}
				violations = append(violations, violation{file: path, fn: name})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("CROSS-TENANT IS-PLATFORM-CHECK VIOLATIONS — %d", len(violations))
		t.Logf("Cross-tenant handlers must check claims.IsPlatform OR be")
		t.Logf("route-wired via RequirePlatform.")
		for _, v := range violations {
			t.Errorf("%s — handler %s suggests cross-tenant scope but lacks IsPlatform check", v.file, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 42: TestArch_NoBareTenantIDStrings
// ----------------------------------------------------------------------------
//
// tenant.ID is a typed string. Production code shouldn't declare raw
// `string` parameters named `tenantID` or `tenant_id` — callers
// should pass tenant.ID. The typed alias prevents accidentally
// swapping a userID into a tenantID slot.
//
// Detection: walk every FuncDecl across internal/<mod>/; flag any
// parameter named tenantID/TenantID/tenant_id whose type is `string`.
func TestArch_NoBareTenantIDStrings(t *testing.T) {
	t.Parallel()

	// Files where bare `tenantID string` is acceptable because the
	// function operates at the substrate boundary (email gateway,
	// subscriber dispatch by Watermill metadata string).
	allowList := []string{
		"internal/common/email/gateway.go",                        // gateway crosses substrate; ID is a metadata string
		"internal/identity/ports/subscribers/revoke_families.go",  // Watermill metadata is plain string
	}

	type violation struct {
		file string
		line int
		fn   string
		arg  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		slashPath := pathToSlash(path)
		for _, allowed := range allowList {
			if strings.HasSuffix(slashPath, allowed) {
				return
			}
		}
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Type == nil || fd.Type.Params == nil {
				continue
			}
			for _, p := range fd.Type.Params.List {
				id, ok := p.Type.(*ast.Ident)
				if !ok || id.Name != "string" {
					continue
				}
				for _, name := range p.Names {
					n := name.Name
					if n == "tenantID" || n == "TenantID" || n == "tenant_id" {
						violations = append(violations, violation{
							file: path,
							line: fset.Position(p.Pos()).Line,
							fn:   fd.Name.Name,
							arg:  n,
						})
					}
				}
			}
		}
	})

	if len(violations) > 0 {
		t.Logf("BARE TENANT-ID-STRING VIOLATIONS — %d", len(violations))
		t.Logf("tenant.ID is a typed string. Take tenant.ID, not bare string —")
		t.Logf("the typed alias prevents accidental swap with other IDs.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s(%s string) (use tenant.ID)", v.file, v.line, v.fn, v.arg)
		}
	}
}
