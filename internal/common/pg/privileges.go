package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
		"shared", "tasks", "dispatch", "orders", "notifications", "companysettings"}
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
// the default. The audit trail is append-only for the application: nothing
// deletes or rewrites a row (ADR 0071: the audit trail is kept forever).
func TablePrivilegeExceptions() map[string][]string {
	return map[string][]string{
		"common.audit_log_entry":           {"SELECT", "INSERT"},
		"common.admin_impersonation_audit": {"SELECT", "INSERT"},
		// Each replay of a timed-out dead letter is kept (review G2b H1), and so is the dead
		// letter itself (review G2c N5). The writer only inserts; the replay job only reads
		// it and inserts its own row, so the role needs nothing more.
		"common.dead_letter_replay": {"SELECT", "INSERT"},
		"common.dead_letter":        {"SELECT", "INSERT"},
		// The policy history is append-only for the application (ADR 0070).
		"companysettings.policy_history": {"SELECT", "INSERT"},
	}
}

// ApplicationRLSFunctions are the helpers in schema app that every RLS policy
// calls (EXECUTE is checked on each).
func ApplicationRLSFunctions() []string {
	return []string{"current_tenant", "is_platform"}
}

// riverLedger is River's own migration ledger; only cmd/migrate (as the owner)
// touches it, so the application role holds nothing on it and the check skips it.
const riverLedger = "river_migration"

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
    WHERE c.relkind IN ('r', 'p')
      AND (n.nspname = ANY($1::text[])
           OR (n.nspname = 'public' AND c.relname LIKE 'river\_%' AND c.relname <> $7::text))
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
SELECT current_user::text, absent, missing FROM (
    SELECT 0 AS ord, true AS absent, 'schema ' || s AS missing
    FROM unnest($1::text[]) AS s
    WHERE NOT EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname = s)
    UNION ALL
    SELECT 0, true, 'function app.' || f || '()'
    FROM unnest($6::text[]) AS f
    WHERE NOT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                      WHERE n.nspname = 'app' AND p.proname = f)
    UNION ALL
    SELECT 1, false, 'USAGE on schema ' || n.nspname
    FROM pg_namespace n
    WHERE n.nspname = ANY($1::text[]) AND NOT has_schema_privilege(n.oid, 'USAGE')
    UNION ALL
    SELECT 2, false, priv || ' on table ' || s || '.' || t FROM need WHERE NOT has_table_privilege(oid, priv)
    UNION ALL
    SELECT 3, false, 'USAGE on sequence ' || n.nspname || '.' || c.relname
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind = 'S'
      AND (n.nspname = ANY($1::text[]) OR (n.nspname = 'public' AND c.relname LIKE 'river\_%'))
      AND NOT EXISTS (SELECT 1 FROM rules WHERE rules.s = n.nspname AND rules.t = '*')
      AND NOT (CASE WHEN c.relkind = 'S' THEN has_sequence_privilege(c.oid, 'USAGE') ELSE true END)
    UNION ALL
    SELECT 4, false, 'EXECUTE on function ' || n.nspname || '.' || p.proname || '()'
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'app' AND p.proname = ANY($6::text[]) AND NOT has_function_privilege(p.oid, 'EXECUTE')
) missing ORDER BY ord, missing LIMIT 1`

// extraQuery returns the FIRST privilege the current role holds on a restricted table
// beyond what its rules allow: an append-only table (the audit trail, the policy
// history, the dead-letter replays) or a table of a read-only schema. $1 application
// schemas; $2/$3/$4 the rules as parallel arrays (schema, table or '*', privilege).
// A role that owns the table, or is a superuser, holds everything implicitly; that is
// the over-privileged-role check's business (a start-up error in production), so it is
// skipped here instead of failing every development database.
const extraQuery = `
WITH rules AS (
    SELECT * FROM unnest($2::text[], $3::text[], $4::text[]) AS r(s, t, priv)
), tbl AS (
    SELECT c.oid, n.nspname AS s, c.relname AS t
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1::text[])
      AND NOT pg_has_role(current_user, c.relowner, 'MEMBER')
)
SELECT tbl.s || '.' || tbl.t, p.priv
FROM tbl CROSS JOIN unnest(ARRAY['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'TRUNCATE', 'TRIGGER', 'pg_write_all_data']) AS p(priv)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname = current_user AND r.rolsuper)
  AND EXISTS (SELECT 1 FROM rules r WHERE r.s = tbl.s AND r.t IN (tbl.t, '*'))
  AND CASE p.priv
        -- membership counts even when NOINHERIT (the role can SET ROLE); it writes every table
        WHEN 'pg_write_all_data' THEN pg_has_role(current_user, 'pg_write_all_data', 'MEMBER')
                                  AND NOT (EXISTS (SELECT 1 FROM rules r WHERE r.s = tbl.s AND r.t IN (tbl.t, '*') AND r.priv = 'UPDATE')
                                       AND EXISTS (SELECT 1 FROM rules r WHERE r.s = tbl.s AND r.t IN (tbl.t, '*') AND r.priv = 'DELETE'))
        -- a column-level grant lets the role change those columns: it counts like the table-level one
        WHEN 'SELECT' THEN has_table_privilege(tbl.oid, p.priv) OR has_any_column_privilege(tbl.oid, p.priv)
        WHEN 'INSERT' THEN has_table_privilege(tbl.oid, p.priv) OR has_any_column_privilege(tbl.oid, p.priv)
        WHEN 'UPDATE' THEN has_table_privilege(tbl.oid, p.priv) OR has_any_column_privilege(tbl.oid, p.priv)
        ELSE has_table_privilege(tbl.oid, p.priv)
      END
  AND (p.priv = 'pg_write_all_data'
       OR NOT EXISTS (SELECT 1 FROM rules r WHERE r.s = tbl.s AND r.t IN (tbl.t, '*') AND r.priv = p.priv))
ORDER BY 1, 2 LIMIT 1`

// excessQuery returns the FIRST reason the current role holds more than the
// application should: superuser and BYPASSRLS each switch row-level security
// off, and the owner of a table is exempt from its policies. Membership counts
// (pg_has_role MEMBER, even for a NOINHERIT role, because it can SET ROLE), so
// a plain role granted one of those roles is caught too. $1 application schemas.
const excessQuery = `
SELECT reason FROM (
    SELECT 0 AS ord, 'is a superuser' AS reason
    FROM pg_roles r WHERE r.rolname = current_user AND r.rolsuper
    UNION ALL
    SELECT 1, 'has BYPASSRLS'
    FROM pg_roles r WHERE r.rolname = current_user AND r.rolbypassrls
    UNION ALL
    SELECT 2, 'is a member of the superuser role ' || quote_ident(r.rolname)
    FROM pg_roles r WHERE r.rolsuper AND r.rolname <> current_user AND pg_has_role(current_user, r.oid, 'MEMBER')
    UNION ALL
    SELECT 3, 'is a member of the BYPASSRLS role ' || quote_ident(r.rolname)
    FROM pg_roles r WHERE r.rolbypassrls AND r.rolname <> current_user AND pg_has_role(current_user, r.oid, 'MEMBER')
    UNION ALL
    SELECT 4, 'owns table ' || n.nspname || '.' || c.relname
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1::text[]) AND pg_has_role(current_user, c.relowner, 'MEMBER')
) x ORDER BY ord, reason LIMIT 1`

// PrivilegeCheck configures [VerifyApplicationPrivileges].
type PrivilegeCheck struct {
	// Production makes a role with TOO MUCH privilege (superuser, BYPASSRLS,
	// owner of an application table) a start-up failure. Outside production it
	// is logged as a warning, because the dev compose database is owned by a
	// superuser. The caller derives it from the existing config.AppConfig.Env
	// ("production"); this package does not read configuration.
	Production bool
	// Logger receives the warning. Required (loggers are injected, never
	// slog.Default()).
	Logger *slog.Logger
}

// VerifyApplicationPrivileges checks the database role the process runs as
// against the privilege contract above, in this order:
//
//  1. every application schema and RLS helper function EXISTS (an unmigrated
//     database yields no rows in any later check, so absence is asserted first);
//  2. the role holds every privilege the application needs, on the application
//     schemas and on River's public.river_* tables, naming the first missing one;
//  3. the role does not hold MORE than it should: a superuser, a BYPASSRLS role
//     or the owner of an application table is exempt from row-level security.
//     That is an error in production and a logged warning elsewhere;
//  4. the role holds no privilege an append-only (or read-only) table does not allow,
//     in every environment: the base grants hand DELETE on every table back, and a
//     repair that stops there would let the application delete the audit trail.
//
// Migration 20260606000001 is a no-op when leadkart_app does not exist yet, and
// default privileges bind to whichever role ran the migration, so a database
// provisioned in the wrong order, or migrated by two different owners, would
// otherwise only fail later, per request, with "permission denied". One or two
// catalog queries, run once at boot by cmd/api and cmd/worker. Messages name the
// role and the privilege, never the DSN or a credential.
func VerifyApplicationPrivileges(ctx context.Context, db interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, check PrivilegeCheck) error {
	if check.Logger == nil {
		return errors.New("pg: PrivilegeCheck.Logger is required")
	}
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
		DefaultTablePrivileges(), ApplicationRLSFunctions(), riverLedger}
	role, absent, missing, found, err := firstRow3(ctx, db, privilegeQuery, args)
	if err != nil {
		return fmt.Errorf("pg: verify application privileges: %w", err)
	}
	if found && absent {
		return fmt.Errorf("pg: database is not migrated: %s does not exist (checked as role %q). "+
			"Run `migrate up` as the owner role before starting the application", missing, role)
	}
	if found {
		return fmt.Errorf("pg: database role %q lacks %s: the grants migration (20260606000001_application_role_grants) did not reach this role "+
			"(it is a no-op when the role does not exist yet, goose records it as applied and will not run it again, and `migrate up` re-applies only River's grants). "+
			"Fix as the OWNER role that ran the migrations: run the Up block of migrations/20260606000001_application_role_grants.sql, of every later *_grants.sql migration (20261123000004_companysettings_grants.sql) "+
			"and of the later migrations that take privileges away again (20261125000003_audit_log_is_kept_forever.sql, 20261125000004_dead_letter_replay.sql) by hand "+
			"(docs/deploy/production-checklist.md, \"The application role\"), then `migrate up`. "+
			"To avoid it: create the role BEFORE the first migration and run every migration as the same owner role", role, missing)
	}

	var reason string
	rows, err := db.Query(ctx, excessQuery, ApplicationSchemas())
	if err != nil {
		return fmt.Errorf("pg: verify application privileges: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&reason); err != nil {
			return fmt.Errorf("pg: verify application privileges: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pg: verify application privileges: %w", err)
	}
	rows.Close() // one statement at a time on the connection
	if reason != "" {
		const consequence = "row-level security does not apply to it, so tenant isolation is not enforced. " +
			"Run the application as a plain role (leadkart_app) that owns nothing; keep the owner role for `migrate` and `bootstrap`"
		if check.Production {
			return errors.New("pg: refusing to start in production: the application's database role " + reason + "; " + consequence)
		}
		check.Logger.WarnContext(ctx, "database role is more privileged than the application needs; "+
			"fine for local development, a start-up error in production",
			"reason", reason, "consequence", consequence)
	}
	return verifyNoExtraPrivileges(ctx, db, args[0].([]string), ruleSchema, ruleTable, rulePriv)
}

// verifyNoExtraPrivileges refuses a role that holds a privilege an append-only or
// read-only table does not allow (see [extraQuery]).
func verifyNoExtraPrivileges(ctx context.Context, db interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, schemas, ruleSchema, ruleTable, rulePriv []string) error {
	rows, err := db.Query(ctx, extraQuery, schemas, ruleSchema, ruleTable, rulePriv)
	if err != nil {
		return fmt.Errorf("pg: verify application privileges: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err()
	}
	var table, priv string
	if err := rows.Scan(&table, &priv); err != nil {
		return fmt.Errorf("pg: verify application privileges: %w", err)
	}
	if priv == "pg_write_all_data" {
		return fmt.Errorf("pg: the application's database role is a member of pg_write_all_data, which writes to every table, table %s (append-only for the application, ADR 0071: data is never deleted) included. "+
			"Fix as the OWNER role: REVOKE pg_write_all_data FROM leadkart_app", table)
	}
	return fmt.Errorf("pg: the application's database role holds %s on table %s, which is append-only for the application (ADR 0071: data is never deleted). "+
		"Fix as the OWNER role: REVOKE %s ON %s FROM leadkart_app, and run the Up block of every migration that takes privileges away "+
		"(20261125000003_audit_log_is_kept_forever.sql, 20261125000004_dead_letter_replay.sql) after any repair that re-runs the grants migrations "+
		"(docs/deploy/production-checklist.md, \"The application role\")", priv, table, priv, table)
}

// firstRow3 runs query and returns its first (text, bool, text) row, if any.
func firstRow3(ctx context.Context, db interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, query string, args []any) (role string, absent bool, missing string, found bool, err error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return "", false, "", false, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&role, &absent, &missing); err != nil {
			return "", false, "", false, err
		}
		found = true
	}
	return role, absent, missing, found, rows.Err()
}
