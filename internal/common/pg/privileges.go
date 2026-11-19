package pg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The privilege contract between the application and its database role
// (leadkart_app). migrations/20260606000001_application_role_grants.sql grants
// it; [VerifyApplicationPrivileges] checks it at start-up; the integration test
// TestAppRoleHasPrivilegesOnEveryApplicationTable pins the database to it. All
// three read the values below, so a change here is a change to all of them.

// ApplicationSchemas are the schemas the application role gets USAGE on.
// Adding a module schema means adding it here AND to the grants migration (its
// own init migration carries the grants, see that migration's header).
func ApplicationSchemas() []string {
	return []string{"app", "common", "identity", "platform", "crm", "inventory",
		"shared", "tasks", "dispatch", "orders", "notifications"}
}

// DefaultTablePrivileges are what the application holds on a table unless the
// schema or the table is listed in the exception maps below.
func DefaultTablePrivileges() []string { return []string{"SELECT", "INSERT", "UPDATE", "DELETE"} }

// SchemaTablePrivileges lists schemas whose tables get FEWER privileges than the
// default, whatever tables they hold now or later (the migration sets default
// privileges for the schema). `shared` is reference data, read-only for tenants.
func SchemaTablePrivileges() map[string][]string {
	return map[string][]string{"shared": {"SELECT"}}
}

// TablePrivilegeExceptions lists single tables that get FEWER privileges than
// the default. The audit trail is append-only for the application: the purge job
// is the only deleter, and nothing rewrites a row.
func TablePrivilegeExceptions() map[string][]string {
	return map[string][]string{
		"common.audit_log_entry":           {"SELECT", "INSERT", "DELETE"},
		"common.admin_impersonation_audit": {"SELECT", "INSERT"},
	}
}

// ApplicationRLSFunctions are the helpers in schema app that every RLS policy
// calls (EXECUTE is checked on each).
func ApplicationRLSFunctions() []string {
	return []string{"current_tenant", "is_platform"}
}

// privilegeQuery returns the FIRST privilege the current role lacks, or no row.
// $1 schemas; $2/$3/$4 rules as parallel arrays (schema, table or '*', privilege);
// $5 the default table privileges. A table's required privileges are its own
// rules, else its schema's '*' rules, else the defaults.
const privilegeQuery = `
WITH rules AS (
    SELECT * FROM unnest($2::text[], $3::text[], $4::text[]) AS r(s, t, priv)
), tbl AS (
    SELECT c.oid, n.nspname AS s, c.relname AS t
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1::text[])
), need AS (
    SELECT tbl.oid, tbl.s, tbl.t, p.priv
    FROM tbl CROSS JOIN LATERAL (
        SELECT priv FROM rules WHERE rules.s = tbl.s AND rules.t = tbl.t
        UNION ALL
        SELECT priv FROM rules WHERE rules.s = tbl.s AND rules.t = '*'
            AND NOT EXISTS (SELECT 1 FROM rules r2 WHERE r2.s = tbl.s AND r2.t = tbl.t)
        UNION ALL
        SELECT d FROM unnest($5::text[]) AS d
            WHERE NOT EXISTS (SELECT 1 FROM rules r3 WHERE r3.s = tbl.s AND r3.t IN (tbl.t, '*'))
    ) p
)
SELECT current_user::text, missing FROM (
    SELECT 1 AS ord, 'USAGE on schema ' || n.nspname AS missing
    FROM pg_namespace n
    WHERE n.nspname = ANY($1::text[]) AND NOT has_schema_privilege(n.oid, 'USAGE')
    UNION ALL
    SELECT 2, priv || ' on table ' || s || '.' || t FROM need WHERE NOT has_table_privilege(oid, priv)
    UNION ALL
    SELECT 3, 'USAGE on sequence ' || n.nspname || '.' || c.relname
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind = 'S' AND n.nspname = ANY($1::text[])
      AND NOT EXISTS (SELECT 1 FROM rules WHERE rules.s = n.nspname AND rules.t = '*')
      AND NOT (CASE WHEN c.relkind = 'S' THEN has_sequence_privilege(c.oid, 'USAGE') ELSE true END)
    UNION ALL
    SELECT 4, 'EXECUTE on function ' || n.nspname || '.' || p.proname || '()'
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'app' AND p.proname = ANY($6::text[]) AND NOT has_function_privilege(p.oid, 'EXECUTE')
) missing ORDER BY ord, missing LIMIT 1`

// VerifyApplicationPrivileges fails when the current database role lacks a
// privilege the application needs on the application schemas, naming the first
// one. Migration 20260606000001 is a no-op when leadkart_app does not exist yet,
// and default privileges bind to whichever role ran the migration, so a
// database provisioned in the wrong order, or migrated by two different owners,
// would otherwise only fail later, per request, with "permission denied". One
// query, run once at boot by cmd/api and cmd/worker. The message names the
// role and the privilege, never the DSN or a credential. A superuser or an
// owner passes (their privileges are implicit), which is what dev runs as.
func VerifyApplicationPrivileges(ctx context.Context, db interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}) error {
	var ruleSchema, ruleTable, rulePriv []string
	for s, privs := range SchemaTablePrivileges() {
		for _, p := range privs {
			ruleSchema, ruleTable, rulePriv = append(ruleSchema, s), append(ruleTable, "*"), append(rulePriv, p)
		}
	}
	for qualified, privs := range TablePrivilegeExceptions() {
		s, t, ok := strings.Cut(qualified, ".")
		if !ok {
			return fmt.Errorf("pg: malformed table privilege exception %q", qualified)
		}
		for _, p := range privs {
			ruleSchema, ruleTable, rulePriv = append(ruleSchema, s), append(ruleTable, t), append(rulePriv, p)
		}
	}

	args := []any{ApplicationSchemas(), ruleSchema, ruleTable, rulePriv,
		DefaultTablePrivileges(), ApplicationRLSFunctions()}
	rows, err := db.Query(ctx, privilegeQuery, args...)
	if err != nil {
		return fmt.Errorf("pg: verify application privileges: %w", err)
	}
	defer rows.Close()
	var role, missing string
	if rows.Next() {
		if err := rows.Scan(&role, &missing); err != nil {
			return fmt.Errorf("pg: verify application privileges: %w", err)
		}
		return fmt.Errorf("pg: database role %q lacks %s: the grants migration (20260606000001) did not reach this role. "+
			"Create the role BEFORE running migrations, run every migration as the same owner role, then re-run `migrate up` "+
			"(it re-applies the grants)", role, missing)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pg: verify application privileges: %w", err)
	}
	return nil
}
