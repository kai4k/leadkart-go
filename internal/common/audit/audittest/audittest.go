// Package audittest holds typed read-side helpers for integration
// tests that need to assert on the common.audit_log_entry
// state — without each test re-deriving the same raw SQL.
//
// Why this package exists: per ADR 0004, all production DB access
// goes through sqlc. Test files were historically exempt from that
// rule (the arch-test sqlc gates scope to non-test files), so each
// integration test that wanted to verify "did the subscriber write
// the audit row?" rolled its own raw `SELECT count(*) FROM
// common.audit_log_entry WHERE action = ...` query.
//
// That blanket exemption was the structural gap that let the
// // arch-test:ignore-err annotation get injected inside a SQL
// string literal (the multi-line backtick made the annotation look
// like a normal trailing comment to a mechanical sweep tool). Typed
// helpers make the bug class impossible — there's no multi-line raw
// string to corrupt.
//
// Tests in non-audit packages MUST use this helper for audit reads.
// The companion arch test TestArch_NoRawSQLInTests enforces it.
package audittest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CountByAction returns the number of audit_log_entry rows matching
// the supplied action with succeeded=true. Used to assert that a
// subscriber / audit middleware wrote the expected event.
//
// Failure (query error) calls t.Fatalf — callers should treat the
// returned count as authoritative.
func CountByAction(t testing.TB, pool *pgxpool.Pool, action string) int64 {
	t.Helper()
	var n int64
	const q = `
		SELECT count(*) FROM common.audit_log_entry
		WHERE  action = $1
		  AND  succeeded = true
	`
	if err := pool.QueryRow(t.Context(), q, action).Scan(&n); err != nil {
		t.Fatalf("audittest.CountByAction(%q): %v", action, err)
	}
	return n
}

// HasAtLeastOneByAction is the canonical wait-until predicate for
// subscriber-commit assertions. Wraps CountByAction in a polling
// shape — returns true once at least one matching row exists.
//
// Designed to be passed to waitFor / similar polling helpers.
func HasAtLeastOneByAction(t testing.TB, pool *pgxpool.Pool, action string) func() bool {
	return func() bool {
		return CountByAction(t, pool, action) >= 1
	}
}

// Row is the part of an audit row the actor and failure assertions read.
type Row struct {
	UserID        *uuid.UUID // nil when the row has no actor
	Succeeded     bool
	FailureReason *string // nil when the row records no failure
}

// RowByAction returns the first audit row for action, and false when none is
// written yet (the caller polls).
func RowByAction(t testing.TB, pool *pgxpool.Pool, action string) (Row, bool) {
	t.Helper()
	var r Row
	const q = `
		SELECT user_id, succeeded, failure_reason FROM common.audit_log_entry
		WHERE  action = $1
		LIMIT  1
	`
	rows, err := pool.Query(t.Context(), q, action)
	if err != nil {
		t.Fatalf("audittest.RowByAction(%q): %v", action, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Row{}, false
	}
	if err := rows.Scan(&r.UserID, &r.Succeeded, &r.FailureReason); err != nil {
		t.Fatalf("audittest.RowByAction(%q) scan: %v", action, err)
	}
	return r, true
}
