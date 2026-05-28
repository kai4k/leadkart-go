// outbox_arch_test.go — fitness function gating the "Transactional
// Outbox with Monotonic Ordering" canon per ADR 0027 + Brandur Leach
// "There's always an events table" + Chris Richardson Microservices
// Patterns ch.3.
//
// THE INVARIANT — every SELECT against any `<schema>.outbox` table
// that has an `ORDER BY` clause MUST include `id` as a tiebreaker.
//
// THE BUG THE TEST PREVENTS — when two events commit with the same
// `created_at` (or `occurred_at`) timestamp, Postgres returns them in
// undefined order on the tie. For a state-machine event sequence
// (e.g. `TenantRegistered` immediately followed by `TenantActivated`
// inside the same tx), the consumer can receive Activated BEFORE
// Registered → downstream state machine breaks. UUIDv7 `id` is
// time-monotonic + provides the required tiebreaker.
//
// SOURCES (the FAANG canon this test enforces):
//   - Brandur Leach "Transactionally Staged Job Drains in Postgres"
//     — drain loop orders by `id`.
//   - Apache Kafka — strict per-partition ordering via offset; the
//     LeadKart equivalent is `id` within a tenant.
//   - Stripe Events API — monotonic `id` per integration; consumers
//     paginate by id.
//   - ThreeDotsLabs Watermill SQL outbox — dispatcher orders by `id`.
//   - Chris Richardson *Microservices Patterns* ch.3 — names the
//     monotonic-id requirement explicitly.
//   - AWS Kinesis Data Streams — SequenceNumber per shard.
//
// arch-test:no-negative-fixture — the assertion target is the SQL
// shape inside the canonical production outbox query files. A negative
// fixture would mean creating a sibling SQL file with the forbidden
// `ORDER BY created_at` (no tiebreaker) — but that file would still
// be picked up by the heuristic + would itself violate the rule the
// test prevents. The path-anchored walk + the single-line ORDER BY
// matcher are the fitness function.
//
// arch-test:no-synctest — purely-static analysis test; no goroutines,
// no time-bound, no DB.

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// orderByOutboxRE captures lines that start an ORDER BY clause in
// a query against a *.outbox table. The matcher is per-line because
// sqlc-formatted queries put ORDER BY on its own line. The earlier
// part of the same query MUST mention `outbox` (case-insensitive)
// so the heuristic doesn't fire on unrelated SELECTs.
//
// Terminators (any one stops the capture):
//   - semicolon (`;`)
//   - newline (`\n`) — handles both .sql files (one-clause-per-line)
//     AND Go raw-string SQL literals (the string literal is on ONE
//     source line, so the newline after the literal terminates).
//   - backtick (`` ` ``) — the Go raw-string terminator; without
//     this, an inline SQL literal like
//     `SELECT ... ORDER BY x, id`, schema)`
//     would capture past the closing backtick + into the Go code.
//   - whitespace + LIMIT/FOR/FETCH — standard SQL clause boundaries.
//
// Non-raw string used because Go raw strings can't contain a backtick.
var orderByOutboxRE = regexp.MustCompile("(?i)ORDER\\s+BY\\s+([^;\\n`]+?)(?:\\s+(?:LIMIT|FOR|FETCH)\\b|[;\\n`]|$)")

// outboxTableRE matches `<schema>.outbox` references — used to scope
// the gate to outbox queries specifically (don't flag ORDER BYs on
// other tables).
var outboxTableRE = regexp.MustCompile(`(?i)\b\w+\.outbox\b`)

// TestArch_OutboxSelectsOrderByMonotonicTiebreaker walks every SQL
// source file (the hand-written .sql files sqlc reads, plus the
// generated .sql.go embedded query strings, plus the Go test helpers
// that emit raw outbox SELECTs) and asserts that any ORDER BY clause
// targeting a `*.outbox` table ends with `id` (or `id DESC`) as a
// tiebreaker. Per ADR 0027 + the citations in the file header.
//
// arch-test:no-negative-fixture — the assertion target is the SQL
// shape inside the canonical production outbox query files. A negative
// fixture would mean creating a sibling SQL file with the forbidden
// `ORDER BY created_at` (no tiebreaker) under
// `testdata/negative/<test>/`. But the path-anchored walk
// (collectOutboxQueryFiles) only inspects the production paths
// `internal/<module>/adapters/sql/*outbox*.sql`, `*/adapters/db/outbox.sql.go`,
// and `common/messaging/messagingtest/outboxtest.go` — a fixture
// outside those paths wouldn't trigger the rule. The fitness function
// IS the path-anchored matcher; the existing 14-violation RED→GREEN
// transition recorded in the commit body is the negative-case proof.
func TestArch_OutboxSelectsOrderByMonotonicTiebreaker(t *testing.T) {
	t.Parallel()

	type violation struct {
		file   string
		line   int
		clause string
	}
	var violations []violation

	// Walk all .sql files under internal/<module>/adapters/sql/ and
	// the test-helper outboxtest.go which builds queries inline.
	candidates := collectOutboxQueryFiles(t)

	for _, path := range candidates {
		src, err := os.ReadFile(path) //nolint:gosec // arch-test fixture path under internal/
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(src)

		// Quick filter: file must touch an outbox table at all.
		if !outboxTableRE.MatchString(body) {
			continue
		}

		// Find every ORDER BY clause + check its column list ends
		// with `id` (or includes id as a tiebreaker).
		matches := orderByOutboxRE.FindAllStringSubmatchIndex(body, -1)
		for _, m := range matches {
			start, end := m[2], m[3]
			clause := strings.TrimSpace(body[start:end])

			// Heuristic: only count this match if the SURROUNDING
			// query mentions outbox. Look back ~500 chars from the
			// ORDER BY to find FROM <schema>.outbox.
			lbStart := start - 500
			if lbStart < 0 {
				lbStart = 0
			}
			lookback := body[lbStart:start]
			if !outboxTableRE.MatchString(lookback) {
				continue
			}

			if !clauseHasIDTiebreaker(clause) {
				violations = append(violations, violation{
					file:   path,
					line:   lineNumber(body, start),
					clause: clause,
				})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("OUTBOX ORDER-BY VIOLATIONS — %d (no `id` tiebreaker; ties on created_at/occurred_at produce undefined order)", len(violations))
		t.Logf("Canon — every outbox SELECT must `ORDER BY <ts>, id` (or `<ts> DESC, id DESC`):")
		t.Logf("  - Brandur Leach 'Transactionally Staged Job Drains in Postgres'")
		t.Logf("  - Kafka per-partition offset / Kinesis SequenceNumber / Stripe event id")
		t.Logf("  - Watermill SQL outbox dispatch loop")
		t.Logf("  - Chris Richardson *Microservices Patterns* ch.3 (Transactional Outbox)")
		for _, v := range violations {
			t.Errorf("%s:%d — ORDER BY %s — missing `id` tiebreaker", v.file, v.line, v.clause)
		}
	}
}

// collectOutboxQueryFiles returns the .sql + Go helper files this gate
// inspects. Centralised so a future module's outbox.sql lands here
// automatically when the path glob picks it up.
func collectOutboxQueryFiles(t *testing.T) []string {
	t.Helper()

	root := internalDir(t)
	var paths []string

	// Production outbox SQL files: internal/<module>/adapters/sql/outbox.sql
	matches, err := filepath.Glob(filepath.Join(root, "*", "adapters", "sql", "*outbox*.sql"))
	if err != nil {
		t.Fatalf("glob outbox.sql: %v", err)
	}
	paths = append(paths, matches...)

	// Test helper that builds raw outbox queries inline.
	helperPath := filepath.Join(root, "common", "messaging", "messagingtest", "outboxtest.go")
	if _, err := os.Stat(helperPath); err == nil {
		paths = append(paths, helperPath)
	}

	// The sqlc-generated `*.sql.go` files mirror the .sql sources — a
	// fix to the .sql regenerates the .go. Including them ensures the
	// gate catches drift if the generated file is hand-edited (a known
	// anti-pattern). Per-module: internal/<module>/adapters/db/outbox.sql.go
	genMatches, err := filepath.Glob(filepath.Join(root, "*", "adapters", "db", "outbox.sql.go"))
	if err != nil {
		t.Fatalf("glob outbox.sql.go: %v", err)
	}
	paths = append(paths, genMatches...)

	return paths
}

// clauseHasIDTiebreaker reports whether the supplied ORDER BY column
// list ends in (or includes as a tiebreaker) `id`. Accepts:
//
//	"created_at, id"               -- canonical
//	"created_at ASC, id ASC"       -- explicit direction
//	"occurred_at DESC, id DESC"    -- DESC tail
//	"id"                           -- id-only (Watermill canon)
//
// Rejects:
//
//	"created_at"                   -- THE BUG
//	"occurred_at DESC"             -- THE BUG (history query)
//	"created_at, topic"            -- has a tiebreaker but not `id`
func clauseHasIDTiebreaker(clause string) bool {
	// Split on commas — last segment is the strongest tiebreaker.
	parts := strings.Split(clause, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	// Strip trailing direction marker + any trailing punctuation.
	last = strings.TrimSuffix(last, ";")
	last = strings.TrimSpace(last)
	tokens := strings.Fields(last)
	if len(tokens) == 0 {
		return false
	}
	col := strings.ToLower(tokens[0])
	return col == "id"
}

// lineNumber returns the 1-indexed line containing position `pos`
// inside `body`. Used to emit human-readable diagnostics.
func lineNumber(body string, pos int) int {
	if pos < 0 || pos > len(body) {
		return 0
	}
	return strings.Count(body[:pos], "\n") + 1
}
