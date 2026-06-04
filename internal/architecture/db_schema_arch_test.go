// db_schema_arch_test.go — Principle 11: DB Schema Hygiene.
//
// Per ADR 0001 (modular monolith — one schema per BC), ADR 0005
// (goose migrations), ADR 0006 (RLS), ADR 0008 (outbox), ADR 0027
// (outbox doubles as audit), ADR 0038 (keyset pagination), Brandur
// "Postgres for everything" + the postgres canonical-naming guide
// (every constraint named explicitly).
//
// Tests in this file:
//   66. TestArch_OutboxTableSchema             (relocated from EDA)
//   67. TestArch_NoCrossSchemaJoins            (relocated from EDA)
//   68. TestArch_EveryTableHasPrimaryKey
//   69. TestArch_TimestampsAreTimestamptz
//   70. TestArch_MoneyColumnsAreBigint
//   71. TestArch_SoftDeleteColumnConsistent
//   72. TestArch_IndexNamingConvention
//   73. TestArch_EveryMigrationHasDownSection
//   74. TestArch_MigrationFilenameFormat
//   75. TestArch_PartialUniqueIndexWithSoftDelete
//   76. TestArch_AuditChainColumnsOnTenantTables
//   77. TestArch_SqlcQueriesParameterized
//   78. TestArch_NoDropTableWithoutADRRef
//   79. TestArch_PgTrgmExtensionDeclared
//   80. TestArch_NoTextWhereVarcharSufficient

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// loadMigrations returns (filename, content) for every migrations/*.sql.
func loadMigrations(t *testing.T) []struct {
	path string
	text string
} {
	t.Helper()
	type entry struct {
		path string
		text string
	}
	var out []entry
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
		out = append(out, entry{path: path, text: string(raw)})
	}
	// Convert anonymous struct to result type.
	result := make([]struct {
		path string
		text string
	}, len(out))
	for i, e := range out {
		result[i] = struct {
			path string
			text string
		}{path: e.path, text: e.text}
	}
	return result
}

// ----------------------------------------------------------------------------
// Test 66: TestArch_OutboxTableSchema
// ----------------------------------------------------------------------------
//
// Per ADR 0064/0067 the outbox is ONE shared relay table (common.outbox)
// drained by the Watermill library Forwarder + watermill-sql queue schema.
// This gate enforces the post-0064 invariants:
//
//   - There is exactly ONE outbox table, and it is common.outbox (the old
//     per-module identity/platform/crm/inventory.outbox tables are gone —
//     the destination topic + tenant/occurred_at/act_* travel in the
//     forwarder envelope / message metadata, not as columns).
//   - common.outbox declares the watermill-sql PostgreSQLQueueSchema column
//     set so the library publisher + subscriber stay drop-in compatible:
//     offset, uuid, payload, metadata, acked, created_at.
//
// Replaces the pre-0064 shape (id/occurred_at/topic/payload/forwarded_at,
// per-module). ADR 0027's "outbox doubles as audit log" was retired by
// ADR 0027 Amd1 + 0064 — audit lives in common.audit_log_entry.
func TestArch_OutboxTableSchema(t *testing.T) {
	t.Parallel()

	tableRE := regexp.MustCompile(`(?is)CREATE TABLE\s+(\w+)\.outbox\s*\((.*?)\);`)
	required := []string{"offset", "uuid", "payload", "metadata", "acked", "created_at"}

	type outboxTable struct {
		file   string
		schema string
		body   string
	}
	var found []outboxTable
	for _, m := range loadMigrations(t) {
		for _, mm := range tableRE.FindAllStringSubmatch(m.text, -1) {
			found = append(found, outboxTable{file: m.path, schema: mm[1], body: strings.ToLower(mm[2])})
		}
	}

	if len(found) == 0 {
		t.Fatal("no CREATE TABLE *.outbox found — expected exactly one (common.outbox per ADR 0064)")
	}
	for _, o := range found {
		if o.schema != "common" {
			t.Errorf("%s — per-module %s.outbox is forbidden; ADR 0064 mandates ONE shared common.outbox relay", o.file, o.schema)
			continue
		}
		var missing []string
		for _, col := range required {
			if !regexp.MustCompile(`(?m)\b` + col + `\b`).MatchString(o.body) {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s — common.outbox missing watermill-sql queue columns: %v (need %v)", o.file, missing, required)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 67: TestArch_NoCrossSchemaJoins (relocated from EDA)
// ----------------------------------------------------------------------------
//
// Per ADR 0001 + ADR 0006: each module owns its Postgres schema; no
// cross-schema joins. Cross-module reads happen via outbox events
// into CQRS projections (ADR 0041), never via direct JOIN.
func TestArch_NoCrossSchemaJoins(t *testing.T) {
	t.Parallel()

	fromRE := regexp.MustCompile(`(?i)\bFROM\s+([a-zA-Z_][a-zA-Z0-9_]*)\.[a-zA-Z_][a-zA-Z0-9_]*`)
	joinRE := regexp.MustCompile(`(?i)\bJOIN\s+([a-zA-Z_][a-zA-Z0-9_]*)\.[a-zA-Z_][a-zA-Z0-9_]*`)

	allowedNonModule := map[string]bool{
		"common":             true,
		"app":                true,
		"pg_catalog":         true,
		"information_schema": true,
		"public":             true,
	}

	type violation struct {
		file   string
		schema string
		clause string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		sqlDir := filepath.Join(internalDir(t), mod, "adapters", "sql")
		entries, err := readDirSafe(sqlDir)
		if err != nil {
			continue
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
			s := stripSQLComments(string(raw))
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
		t.Logf("Per ADR 0001 + 0006: each module owns its schema. Cross-")
		t.Logf("module reads MUST flow through outbox → subscriber → projection.")
		for _, v := range violations {
			t.Errorf("%s — %s clause references schema %q", v.file, v.clause, v.schema)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 68: TestArch_EveryTableHasPrimaryKey
// ----------------------------------------------------------------------------
//
// Every CREATE TABLE contains PRIMARY KEY (column-level or
// table-level). Without one, postgres queries can return duplicate
// rows + replication ordering breaks.
func TestArch_EveryTableHasPrimaryKey(t *testing.T) {
	t.Parallel()

	tableRE := regexp.MustCompile(`(?is)CREATE TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)\s*\((.*?)\);`)

	type violation struct {
		file  string
		table string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		for _, mm := range tableRE.FindAllStringSubmatch(m.text, -1) {
			table := mm[1]
			body := strings.ToUpper(mm[2])
			if !strings.Contains(body, "PRIMARY KEY") {
				violations = append(violations, violation{file: m.path, table: table})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("MISSING PRIMARY KEY VIOLATIONS — %d", len(violations))
		t.Logf("Every CREATE TABLE must declare a PRIMARY KEY.")
		for _, v := range violations {
			t.Errorf("%s — table %s has no PRIMARY KEY", v.file, v.table)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 69: TestArch_TimestampsAreTimestamptz
// ----------------------------------------------------------------------------
//
// No `timestamp` column type without `tz`. Catches `timestamp without
// time zone` — almost always a bug; per Brandur "Postgres for
// everything" canon, every timestamp is timestamptz.
func TestArch_TimestampsAreTimestamptz(t *testing.T) {
	t.Parallel()

	// Match `<column-name> timestamp` NOT followed by `tz`.
	badRE := regexp.MustCompile(`(?im)\btimestamp\s+without\s+time\s+zone\b`)
	bareRE := regexp.MustCompile(`(?im)\b(\w+)\s+timestamp(?:\s|,|\))(?:[^t]|$)`)

	type violation struct {
		file string
		col  string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		stripped := stripSQLComments(m.text)
		// Pass 1: explicit `timestamp without time zone`.
		if badRE.MatchString(stripped) {
			violations = append(violations, violation{file: m.path, col: "<timestamp without time zone>"})
		}
		// Pass 2: bare `<col> timestamp` shape — much harder. Be conservative:
		// look for the literal substring " timestamp " (space-padded), and
		// exclude lines that include "timestamptz".
		for _, mm := range bareRE.FindAllStringSubmatch(stripped, -1) {
			col := mm[1]
			// Skip the function-arg case where the "timestamp" is a parameter
			// type to a function definition (rare).
			if strings.Contains(strings.ToLower(mm[0]), "timestamptz") {
				continue
			}
			violations = append(violations, violation{file: m.path, col: col})
		}
	}

	if len(violations) > 0 {
		t.Logf("BARE-TIMESTAMP VIOLATIONS — %d", len(violations))
		t.Logf("Per Brandur: every timestamp is timestamptz. Bare `timestamp`")
		t.Logf("is `timestamp without time zone` (a near-universal foot-gun).")
		for _, v := range violations {
			t.Errorf("%s — column %s typed bare timestamp", v.file, v.col)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 70: TestArch_MoneyColumnsAreBigint
// ----------------------------------------------------------------------------
//
// Columns whose name matches money-vocabulary (amount, price, cost,
// balance, fee, paise, charge, refund, gst_amount, tax_amount) AND
// aren't enum/text must be `bigint`. Per Stripe canon: money is
// int64 in smallest unit.
func TestArch_MoneyColumnsAreBigint(t *testing.T) {
	t.Parallel()

	moneyColRE := regexp.MustCompile(`(?im)^\s+(\w*(?:amount|price|cost|balance|fee|paise|fare|charge|refund|gst_amount|tax_amount)\w*)\s+(\w+)`)

	type violation struct {
		file string
		col  string
		typ  string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		stripped := stripSQLComments(m.text)
		for _, mm := range moneyColRE.FindAllStringSubmatch(stripped, -1) {
			col := strings.ToLower(mm[1])
			typ := strings.ToLower(mm[2])
			// Accept bigint/int8/numeric (which we treat as policy
			// equivalent for tax rates expressed as bps).
			if typ == "bigint" || typ == "int8" || typ == "numeric" {
				continue
			}
			// Allow text-shaped enums (e.g. a column called
			// `refund_status` of type varchar — not money).
			if strings.HasSuffix(col, "_kind") || strings.HasSuffix(col, "_status") || strings.HasSuffix(col, "_type") {
				continue
			}
			violations = append(violations, violation{file: m.path, col: col, typ: typ})
		}
	}

	if len(violations) > 0 {
		t.Logf("MONEY-COLUMN-NOT-BIGINT VIOLATIONS — %d", len(violations))
		t.Logf("Per Stripe canon: money is int64 in smallest unit.")
		for _, v := range violations {
			t.Errorf("%s — column %s typed %s (expected bigint)", v.file, v.col, v.typ)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 71: TestArch_SoftDeleteColumnConsistent
// ----------------------------------------------------------------------------
//
// If a table declares `is_deleted bool`, it MUST also declare
// `deleted_at timestamptz`. The bool says WHAT; the timestamp says
// WHEN — without it, audit queries can't reconstruct the deletion
// timeline.
func TestArch_SoftDeleteColumnConsistent(t *testing.T) {
	t.Parallel()

	tableRE := regexp.MustCompile(`(?is)CREATE TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)\s*\((.*?)\);`)

	type violation struct {
		file  string
		table string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		for _, mm := range tableRE.FindAllStringSubmatch(m.text, -1) {
			table := mm[1]
			body := strings.ToLower(mm[2])
			hasIsDel := regexp.MustCompile(`\bis_deleted\s+bool`).MatchString(body)
			hasDelAt := regexp.MustCompile(`\bdeleted_at\b`).MatchString(body)
			if hasIsDel && !hasDelAt {
				violations = append(violations, violation{file: m.path, table: table})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("SOFT-DELETE INCONSISTENCY VIOLATIONS — %d", len(violations))
		t.Logf("Tables with is_deleted MUST also have deleted_at timestamptz.")
		for _, v := range violations {
			t.Errorf("%s — table %s has is_deleted but no deleted_at", v.file, v.table)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 72: TestArch_IndexNamingConvention
// ----------------------------------------------------------------------------
//
// Every CREATE INDEX / CREATE UNIQUE INDEX name matches `^(idx|uq)_`.
// Constraints use `^(fk|chk|uq|pk)_`. Postgres auto-generated names
// are forbidden — they break grep + migration diffs.
func TestArch_IndexNamingConvention(t *testing.T) {
	t.Parallel()

	indexRE := regexp.MustCompile(`(?im)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)
	idxPat := regexp.MustCompile(`^(idx|uq)_[a-z0-9_]+$`)

	type violation struct {
		file string
		name string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		stripped := stripSQLComments(m.text)
		for _, mm := range indexRE.FindAllStringSubmatch(stripped, -1) {
			name := mm[1]
			if !idxPat.MatchString(name) {
				violations = append(violations, violation{file: m.path, name: name})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("INDEX-NAMING VIOLATIONS — %d", len(violations))
		t.Logf("Indexes use `idx_<table>_<cols>` (or `uq_<table>_<cols>` for unique).")
		for _, v := range violations {
			t.Errorf("%s — index name %q doesn't match ^(idx|uq)_", v.file, v.name)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 73: TestArch_EveryMigrationHasDownSection
// ----------------------------------------------------------------------------
//
// Every migration file contains both `-- +goose Up` AND `-- +goose Down`.
// Per ADR 0005: goose migrations are reversible (or at least declare
// the down direction explicitly, even if it's a no-op + a comment).
func TestArch_EveryMigrationHasDownSection(t *testing.T) {
	t.Parallel()

	type violation struct {
		file    string
		missing string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		if !strings.Contains(m.text, "-- +goose Up") {
			violations = append(violations, violation{file: m.path, missing: "-- +goose Up"})
		}
		if !strings.Contains(m.text, "-- +goose Down") {
			violations = append(violations, violation{file: m.path, missing: "-- +goose Down"})
		}
	}

	if len(violations) > 0 {
		t.Logf("GOOSE-SECTION VIOLATIONS — %d", len(violations))
		t.Logf("Every migration declares `-- +goose Up` AND `-- +goose Down`.")
		for _, v := range violations {
			t.Errorf("%s — missing %s", v.file, v.missing)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 74: TestArch_MigrationFilenameFormat
// ----------------------------------------------------------------------------
//
// migrations/*.sql filenames match `^\d{14}_[a-z0-9_]+\.sql$` — the
// canonical goose UTC timestamp + snake-case label.
func TestArch_MigrationFilenameFormat(t *testing.T) {
	t.Parallel()

	pat := regexp.MustCompile(`^\d{14}_[a-z0-9_]+\.sql$`)

	type violation struct{ file string }
	var violations []violation

	entries, err := readDirSafe(migrationsDir(t))
	if err != nil {
		t.Fatalf("read migrations/: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if !pat.MatchString(e.Name()) {
			violations = append(violations, violation{file: e.Name()})
		}
	}

	if len(violations) > 0 {
		t.Logf("MIGRATION-FILENAME VIOLATIONS — %d", len(violations))
		t.Logf("Pattern: <14-digit utc>_<snake_case>.sql")
		for _, v := range violations {
			t.Errorf("%s — doesn't match %s", v.file, pat.String())
		}
	}
}

// ----------------------------------------------------------------------------
// Test 75: TestArch_PartialUniqueIndexWithSoftDelete
// ----------------------------------------------------------------------------
//
// For tables with soft-delete, every CREATE UNIQUE INDEX on a column
// that overlaps the deletable surface includes a `WHERE NOT
// is_deleted` (or `WHERE deleted_at IS NULL`) clause. Without it,
// restoring a row collides with the active duplicate.
//
// Heuristic: per migration, if the file declares ANY `is_deleted`
// column, every CREATE UNIQUE INDEX in the same file must end with
// a WHERE clause referencing is_deleted / deleted_at.
func TestArch_PartialUniqueIndexWithSoftDelete(t *testing.T) {
	t.Parallel()

	uniqueIdxRE := regexp.MustCompile(`(?is)CREATE\s+UNIQUE\s+INDEX\s+(\w+)[^;]+`)
	whereSoftDelRE := regexp.MustCompile(`(?i)WHERE\s+(NOT\s+is_deleted|deleted_at\s+IS\s+NULL|is_deleted\s*=\s*false)`)
	hasIsDeletedRE := regexp.MustCompile(`(?i)\bis_deleted\b`)

	// Index allow-list: indexes that are intentionally GLOBAL UNIQUE
	// (e.g. session-id uniqueness across all rows including deleted).
	allowList := map[string]bool{
		// Populate as encountered; empty for now.
	}

	type violation struct {
		file string
		idx  string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		stripped := stripSQLComments(m.text)
		if !hasIsDeletedRE.MatchString(stripped) {
			continue
		}
		for _, mm := range uniqueIdxRE.FindAllStringSubmatch(stripped, -1) {
			idxName := mm[1]
			if allowList[idxName] {
				continue
			}
			body := mm[0]
			if whereSoftDelRE.MatchString(body) {
				continue
			}
			violations = append(violations, violation{file: m.path, idx: idxName})
		}
	}

	if len(violations) > 0 {
		t.Logf("PARTIAL-UNIQUE-INDEX VIOLATIONS — %d", len(violations))
		t.Logf("Per Brandur \"Postgres unique indexes for distributed locks\" + ADR 0027:")
		t.Logf("  UNIQUE INDEXes on soft-deletable tables MUST carry a partial WHERE clause")
		t.Logf("  (`WHERE NOT is_deleted` / `WHERE deleted_at IS NULL`) so restoring a row")
		t.Logf("  doesn't collide with the still-indexed soft-deleted ghost.")
		t.Logf("  Canonical fix template lives in migrations/20260603000302_partial_unique_indexes_soft_delete.sql.")
		for _, v := range violations {
			t.Errorf("%s — unique index %q lacks `WHERE NOT is_deleted` / `WHERE deleted_at IS NULL`", v.file, v.idx)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 76: TestArch_AuditChainColumnsOnTenantTables
// ----------------------------------------------------------------------------
//
// Wave 1.5 audit-chain discipline (migration 20260507000008 + ADR 0058
// + ADR 0027). Every tenant-scoped mutable aggregate MUST carry
// `created_by_membership_id uuid` so the "who created this row" question
// is answered declaratively, not reconstructed from event streams
// (Stripe / Plaid / Salesforce shape).
//
// Predicate: walk every migrations/*.sql; for every CREATE TABLE that
// declares a `tenant_id` column, the table must EITHER declare
// `created_by_membership_id` inside the same CREATE TABLE body OR be
// extended later via `ALTER TABLE ... ADD COLUMN ... created_by_membership_id`
// in some subsequent migration.
//
// Allow-list (explicit exemptions): global aggregates, session infra,
// append-only ledgers, outbox tables, and the rolehierarchy / permission*
// family which carry semantically-equivalent audit columns under
// different names (`established_by_membership_id`, etc.). Adding to
// the allow-list requires an ADR-level justification in the PR.
func TestArch_AuditChainColumnsOnTenantTables(t *testing.T) {
	t.Parallel()

	// Tables that carry a tenant_id column but are exempt from the
	// `created_by_membership_id` requirement for documented reasons.
	// Keep grep-able + cited.
	allowList := map[string]string{
		// Global / session / infra:
		"identity.tenants":                 "global aggregate (each row IS a tenant)",
		"identity.persons":                 "global identity (NOT tenant-scoped)",
		"identity.refresh_token_families":  "session infrastructure (RFC 9700)",
		"identity.refresh_tokens":          "session infrastructure (RFC 9700)",
		"identity.auth_routing":            "cross-tenant routing table (non-RLS)",
		"identity.processed_messages":      "messaging infra (Watermill bookkeeping)",
		"identity.outbox":                  "outbox / audit log (ADR 0027)",
		"platform.outbox":                  "outbox / audit log (ADR 0027)",
		"inventory.outbox":                 "outbox / audit log (ADR 0027)",
		"crm.outbox":                       "outbox / audit log (ADR 0027)",
		"common.audit_log_entry":           "audit log sink",
		"common.admin_impersonation_audit": "audit log (ADR 0045)",
		"common.command_idempotency":       "idempotency infra",
		// Permission / hierarchy family (carry their own audit columns):
		"identity.membership_permission_overrides": "permission* family (overlay state)",
		"identity.permission_requests":             "permission* family (request workflow, has approver_membership_id)",
		"identity.role_hierarchy_edges":            "rolehierarchy* family (carries established_by_membership_id)",
		// Append-only ledgers / event-stream aggregates:
		"inventory.stock_movements":   "event-stream aggregate (carries actor_membership_id)",
		"inventory.alert_emissions":   "system-emitted dedup ledger — no human author; subject_id+kind+day PK",
		"platform.verification_calls": "append-only ledger (carries logged_by_membership_id)",
		"crm.call_logs":               "append-only call audit (carries logged_by_membership_id) per ADR 0060",
		"crm.assignment_history":      "append-only assignment audit (carries assigned_by_membership_id) per ADR 0060",
		// Platform globals:
		"platform.platform_leads":      "marketplace global (carries verified_by_membership_id + sold_to_membership_id)",
		"platform.lead_credits":        "balance aggregate (no creation event)",
		"platform.unverified_contacts": "platform-only Lead Agent queue (already carries created_by_membership_id NOT NULL)",
	}

	// Match CREATE TABLE <schema>.<name> ( ... ); for any schema.
	tableRE := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)\s*\((.*?)\);`)
	// Match `tenant_id` declared as a column inside the CREATE TABLE body.
	hasTenantColRE := regexp.MustCompile(`(?im)^\s*tenant_id\s+uuid\b`)
	// Match `created_by_membership_id` inside the CREATE TABLE body.
	hasCreatedByInBody := regexp.MustCompile(`(?im)\bcreated_by_membership_id\b`)
	// Match an ALTER TABLE ... ADD COLUMN ... created_by_membership_id ...
	// targeting the given <schema>.<table>. We build the regex per-table
	// so we can prove the column lands on this specific table.
	alterAddCol := func(qualified string) *regexp.Regexp {
		return regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?` +
			regexp.QuoteMeta(qualified) +
			`\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?created_by_membership_id\b`)
	}

	migs := loadMigrations(t)

	type violation struct {
		file  string
		table string
	}
	var violations []violation

	for _, m := range migs {
		stripped := stripSQLComments(m.text)
		for _, mm := range tableRE.FindAllStringSubmatch(stripped, -1) {
			table := strings.ToLower(mm[1])
			body := mm[2]
			if !hasTenantColRE.MatchString(body) {
				continue
			}
			if _, exempt := allowList[table]; exempt {
				continue
			}
			if hasCreatedByInBody.MatchString(body) {
				continue
			}
			// Search every migration (including ones earlier than `m`,
			// since CI re-applies the full stack) for an ALTER TABLE that
			// adds the column to this specific table.
			found := false
			altRE := alterAddCol(table)
			for _, other := range migs {
				if altRE.MatchString(stripSQLComments(other.text)) {
					found = true
					break
				}
			}
			if !found {
				violations = append(violations, violation{file: m.path, table: table})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("AUDIT-CHAIN COLUMN VIOLATIONS — %d", len(violations))
		t.Logf("Per Wave 1.5 (migration 20260507000008) + ADR 0027 + ADR 0058:")
		t.Logf("  Every tenant-scoped mutable aggregate MUST carry")
		t.Logf("  `created_by_membership_id uuid` so authorship is part of row state.")
		t.Logf("  Add the column via a goose migration in the migrations/ tree,")
		t.Logf("  or — if the table is genuinely exempt — extend the allowList in")
		t.Logf("  this test with a one-line rationale citing the ADR / pattern.")
		for _, v := range violations {
			t.Errorf("%s — table %s lacks created_by_membership_id (no CREATE TABLE column + no later ALTER TABLE ADD COLUMN)", v.file, v.table)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 77: TestArch_SqlcQueriesParameterized
// ----------------------------------------------------------------------------
//
// .sql files under internal/<mod>/adapters/sql/ contain no `${...}`
// interpolation (Go template syntax leaked into SQL). Every dynamic
// value goes through a sqlc parameter ($1, $2, ...).
func TestArch_SqlcQueriesParameterized(t *testing.T) {
	t.Parallel()

	templateRE := regexp.MustCompile(`\$\{[^}]+\}`)

	type violation struct {
		file string
		hit  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		sqlDir := filepath.Join(internalDir(t), mod, "adapters", "sql")
		walkFilesByExt(t, sqlDir, ".sql", func(path string, src []byte) {
			for _, m := range templateRE.FindAllString(string(src), -1) {
				violations = append(violations, violation{file: path, hit: m})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("SQL TEMPLATE-INTERPOLATION VIOLATIONS — %d", len(violations))
		t.Logf("sqlc queries use $1, $2, ... — never Go-template ${...}.")
		for _, v := range violations {
			t.Errorf("%s — %s", v.file, v.hit)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 78: TestArch_NoDropTableWithoutADRRef
// ----------------------------------------------------------------------------
//
// Any DROP TABLE in a migration must appear in a file whose comments
// reference an ADR # (`ADR 0XXX`). Schema removals carry blast radius —
// requiring an ADR reference forces the discussion.
func TestArch_NoDropTableWithoutADRRef(t *testing.T) {
	t.Parallel()

	dropRE := regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)
	adrRE := regexp.MustCompile(`ADR\s*\d{4}`)

	type violation struct{ file string }
	var violations []violation

	for _, m := range loadMigrations(t) {
		// DROP TABLE inside `-- +goose Down` is a rollback declaration
		// (not a destructive UP migration). We only flag DROPs in the
		// UP section.
		upSection := m.text
		if idx := strings.Index(m.text, "-- +goose Down"); idx >= 0 {
			upSection = m.text[:idx]
		}
		if !dropRE.MatchString(upSection) {
			continue
		}
		if adrRE.MatchString(m.text) {
			continue
		}
		violations = append(violations, violation{file: m.path})
	}

	if len(violations) > 0 {
		t.Logf("DROP TABLE WITHOUT ADR REFERENCE VIOLATIONS — %d", len(violations))
		t.Logf("Schema removals need an ADR # in a comment somewhere in the migration.")
		for _, v := range violations {
			t.Errorf("%s — DROP TABLE without ADR reference", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 79: TestArch_PgTrgmExtensionDeclared
// ----------------------------------------------------------------------------
//
// If any migration creates a GIN index using gin_trgm_ops, an earlier
// migration must declare `CREATE EXTENSION IF NOT EXISTS pg_trgm`.
// Per ADR 0040.
func TestArch_PgTrgmExtensionDeclared(t *testing.T) {
	t.Parallel()

	migs := loadMigrations(t)

	usesTrgm := false
	declaresTrgm := false
	for _, m := range migs {
		if strings.Contains(strings.ToLower(m.text), "gin_trgm_ops") {
			usesTrgm = true
		}
		if regexp.MustCompile(`(?i)CREATE\s+EXTENSION\s+(IF\s+NOT\s+EXISTS\s+)?pg_trgm`).MatchString(m.text) {
			declaresTrgm = true
		}
	}

	if usesTrgm && !declaresTrgm {
		t.Errorf("migrations use gin_trgm_ops but no CREATE EXTENSION pg_trgm is declared")
	}
}

// ----------------------------------------------------------------------------
// Test 80: TestArch_NoTextWhereVarcharSufficient
// ----------------------------------------------------------------------------
//
// Columns whose names end in `_code` / `_slug` / `_kind` must be
// `varchar(N)` not bare `text`. The bounded type enforces a
// length-limit invariant at the DB layer.
func TestArch_NoTextWhereVarcharSufficient(t *testing.T) {
	t.Parallel()

	// Match `<col-name> text` shape where the col ends in _code|_slug|_kind.
	badRE := regexp.MustCompile(`(?im)\b(\w+(?:_code|_slug|_kind))\s+text\b`)

	// Columns where the bounded length isn't tightly knowable + `text`
	// is preferred (Postgres docs note `varchar(N)` vs `text` has zero
	// perf difference; the bound is purely for invariant enforcement).
	allowedColumns := map[string]bool{
		"hsn_code":                 true, // Indian HSN codes: 4/6/8 digits but format is flexible
		"outcome_code":             true, // free-form call outcome label (e.g. "called_no_answer")
		"admin_address_state_code": true, // ISO 3166-2 state code (2-3 chars) but stored from form input
	}

	type violation struct {
		file string
		col  string
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		stripped := stripSQLComments(m.text)
		for _, mm := range badRE.FindAllStringSubmatch(stripped, -1) {
			col := strings.ToLower(mm[1])
			if allowedColumns[col] {
				continue
			}
			violations = append(violations, violation{file: m.path, col: col})
		}
	}

	if len(violations) > 0 {
		t.Logf("TEXT-FOR-BOUNDED-COLUMN VIOLATIONS — %d", len(violations))
		t.Logf("Columns ending _code/_slug/_kind are bounded — use varchar(N).")
		for _, v := range violations {
			t.Errorf("%s — column %s typed bare text", v.file, v.col)
		}
	}
}

// ============================================================================
// Principle B: goose-migration discipline (6 tests added per the comprehensive
// catalog brief).
//
// goose's idioms are subtle and easy to violate silently — multi-statement
// DDL needs `+goose StatementBegin/End`, nested transactions break the
// implicit migration tx, and destructive ops (DROP COLUMN / DROP CONSTRAINT)
// without an ADR reference are accident-class events.
// ============================================================================

// ----------------------------------------------------------------------------
// B1: TestArch_MigrationUsesStatementBegin
// ----------------------------------------------------------------------------
//
// goose runs each top-level statement separately unless wrapped in
// `+goose StatementBegin / +goose StatementEnd`. Multi-statement DDL
// blocks (DO $$ ... $$, CREATE FUNCTION ... LANGUAGE plpgsql AS $$
// ... $$;) MUST use the wrapper or goose splits them on the first
// semicolon and the migration explodes.
//
// Predicate: every `DO $$` or `LANGUAGE plpgsql` block in any migration
// must be preceded within 5 lines by `-- +goose StatementBegin`.
func TestArch_MigrationUsesStatementBegin(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, m := range loadMigrations(t) {
		lines := strings.Split(m.text, "\n")
		// Walk the file once tracking the most-recent StatementBegin /
		// StatementEnd. A multi-statement DDL site (DO $$ /
		// LANGUAGE plpgsql) is in a wrapped block iff the most recent
		// marker BEFORE that line is a Begin (not an End).
		inBlock := false
		for i, ln := range lines {
			if strings.Contains(ln, "+goose StatementBegin") {
				inBlock = true
				continue
			}
			if strings.Contains(ln, "+goose StatementEnd") {
				inBlock = false
				continue
			}
			lower := strings.ToLower(ln)
			needsWrap := strings.Contains(lower, "do $$") ||
				strings.Contains(lower, "language plpgsql")
			if !needsWrap {
				continue
			}
			if !inBlock {
				violations = append(violations, violation{file: m.path, line: i + 1})
			}
		}
	}
	if len(violations) > 0 {
		for _, v := range violations {
			t.Errorf("%s:%d — multi-statement DDL missing +goose StatementBegin/End wrapper",
				filepath.Base(v.file), v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// B2: TestArch_NoNestedTransactionsInMigrations
// ----------------------------------------------------------------------------
//
// goose wraps each migration in an implicit transaction (unless
// `-- +goose NO TRANSACTION` is declared). Explicit BEGIN / COMMIT
// inside a migration body breaks the implicit tx and produces
// surprising partial-apply behaviour.
//
// Allow-list: the literal token `BEGIN` may appear inside `EXCEPTION
// WHEN ... THEN BEGIN ... END` blocks (PL/pgSQL exception handlers)
// or `START TRANSACTION` — but bare `BEGIN;` on its own line is
// banned.
func TestArch_NoNestedTransactionsInMigrations(t *testing.T) {
	t.Parallel()

	// Bare BEGIN; on its own line (any leading whitespace). Excludes
	// inline BEGIN inside DO blocks where it's PL/pgSQL syntax.
	bareBeginRE := regexp.MustCompile(`(?im)^\s*BEGIN\s*;\s*$`)
	bareCommitRE := regexp.MustCompile(`(?im)^\s*COMMIT\s*;\s*$`)

	var violations []string
	for _, m := range loadMigrations(t) {
		body := stripSQLComments(m.text)
		if bareBeginRE.MatchString(body) || bareCommitRE.MatchString(body) {
			violations = append(violations, filepath.Base(m.path))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("migration declares explicit BEGIN/COMMIT — goose already wraps in tx (ADR 0005):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// B3: TestArch_NoDropColumnWithoutADRRef
// ----------------------------------------------------------------------------
//
// DROP COLUMN is destructive + irreversible against production data.
// Every occurrence MUST reference the ADR justifying the removal
// (Brandur "schema changes" canon — a paper trail is non-optional).
//
// Looks for an `ADR-NNNN` token in the same migration file as any
// `DROP COLUMN` (case-insensitive). Only `-- +goose Up` block
// occurrences are flagged; Down sections legitimately drop the
// columns the Up section added.
func TestArch_NoDropColumnWithoutADRRef(t *testing.T) {
	t.Parallel()

	dropRE := regexp.MustCompile(`(?i)DROP\s+COLUMN`)
	adrRE := regexp.MustCompile(`(?i)ADR[\s-]?\d{3,4}`)

	var violations []string
	for _, m := range loadMigrations(t) {
		up, _ := splitGooseUpDown(m.text)
		stripped := stripSQLComments(up)
		if !dropRE.MatchString(stripped) {
			continue
		}
		// ADR ref check uses the whole file body (header comment is
		// the canonical place to cite).
		if !adrRE.MatchString(m.text) {
			violations = append(violations, filepath.Base(m.path))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("DROP COLUMN without ADR reference (paper-trail required):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// B4: TestArch_NoDropConstraintWithoutADRRef
// ----------------------------------------------------------------------------
//
// Same canon as B3 but for DROP CONSTRAINT: removing FKs / unique
// indexes / check constraints is invariant-breaking — must cite ADR.
func TestArch_NoDropConstraintWithoutADRRef(t *testing.T) {
	t.Parallel()

	dropRE := regexp.MustCompile(`(?i)DROP\s+CONSTRAINT`)
	adrRE := regexp.MustCompile(`(?i)ADR[\s-]?\d{3,4}`)

	var violations []string
	for _, m := range loadMigrations(t) {
		up, _ := splitGooseUpDown(m.text)
		stripped := stripSQLComments(up)
		if !dropRE.MatchString(stripped) {
			continue
		}
		if !adrRE.MatchString(m.text) {
			violations = append(violations, filepath.Base(m.path))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("DROP CONSTRAINT without ADR reference (paper-trail required):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// B5: TestArch_DataLiftHasRollbackPath
// ----------------------------------------------------------------------------
//
// Migrations that perform a data-lift in the Up section (INSERT INTO
// new_table SELECT ... FROM old_table) MUST have a corresponding
// reversal in the Down section. If we revert the schema, the data
// has to come back too — otherwise rollback is silent data loss.
//
// Heuristic: Up contains `INSERT INTO ... SELECT ... FROM` AND Down
// contains neither `INSERT INTO` nor an explicit
// `-- arch-test:data-lift-irreversible <reason>` opt-out.
func TestArch_DataLiftHasRollbackPath(t *testing.T) {
	t.Parallel()

	liftRE := regexp.MustCompile(`(?is)INSERT\s+INTO\s+\S+\s*\([^)]*\)\s*SELECT`)

	var violations []string
	for _, m := range loadMigrations(t) {
		up, down := splitGooseUpDown(m.text)
		upBody := stripSQLComments(up)
		if !liftRE.MatchString(upBody) {
			continue
		}
		if strings.Contains(m.text, "arch-test:data-lift-irreversible") {
			continue
		}
		downBody := stripSQLComments(down)
		if !strings.Contains(strings.ToUpper(downBody), "INSERT INTO") &&
			!strings.Contains(strings.ToUpper(downBody), "UPDATE ") {
			violations = append(violations, filepath.Base(m.path))
		}
	}
	if len(violations) > 0 {
		t.Fatalf("migration data-lift has no rollback path in Down section (silent data loss on revert):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// B6: TestArch_ConcurrentIndexOnHotTables
// ----------------------------------------------------------------------------
//
// Tables tagged `-- arch-test:hot-table` (high-write tables where a
// CREATE INDEX would lock-out writes) must use `CREATE INDEX
// CONCURRENTLY`. Brandur "Postgres for everything" canon — for any
// table large enough to matter, concurrent is the default.
//
// Predicate: every CREATE INDEX in a migration that ALSO creates a
// table carrying the `-- arch-test:hot-table` marker must use
// CONCURRENTLY.
func TestArch_ConcurrentIndexOnHotTables(t *testing.T) {
	t.Parallel()

	createIdxRE := regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+ON\s+([\w.]+)`)
	hotMarker := "arch-test:hot-table"

	// Build set of hot tables (any table whose CREATE TABLE block in
	// any migration carries the marker comment on the prior 3 lines).
	tableRE := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([\w.]+)`)
	hot := map[string]struct{}{}
	for _, m := range loadMigrations(t) {
		lines := strings.Split(m.text, "\n")
		for i, ln := range lines {
			mm := tableRE.FindStringSubmatch(ln)
			if mm == nil {
				continue
			}
			// Look back 4 lines for the marker.
			for j := i - 1; j >= 0 && j > i-5; j-- {
				if strings.Contains(lines[j], hotMarker) {
					hot[strings.ToLower(mm[1])] = struct{}{}
					break
				}
			}
		}
	}

	if len(hot) == 0 {
		// No hot-table markers in the codebase yet — test is satisfied
		// vacuously. The presence of the test is the institutional
		// lever: as soon as one CRM lead-table gets the marker the
		// gate is live.
		return
	}

	var bad []string
	for _, m := range loadMigrations(t) {
		for _, mm := range createIdxRE.FindAllStringSubmatch(m.text, -1) {
			tbl := strings.ToLower(mm[2])
			if _, ok := hot[tbl]; !ok {
				continue
			}
			whole := mm[0]
			if !strings.Contains(strings.ToUpper(whole), "CONCURRENTLY") {
				bad = append(bad, filepath.Base(m.path)+": "+mm[1]+" on "+tbl)
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("CREATE INDEX on hot-table missing CONCURRENTLY (would lock writers):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// splitGooseUpDown splits a goose migration body into Up + Down
// halves at the `-- +goose Down` marker. Returns (up, down).
func splitGooseUpDown(body string) (string, string) {
	downRE := regexp.MustCompile(`(?im)^\s*--\s*\+goose\s+Down\s*$`)
	loc := downRE.FindStringIndex(body)
	if loc == nil {
		return body, ""
	}
	return body[:loc[0]], body[loc[0]:]
}
