// Package messagingtest holds typed read-side helpers for
// integration tests that assert on outbox + inbox (processed_messages)
// state.
//
// Outbox tables (identity.outbox, inventory.outbox, platform.outbox)
// are FORCE ROW LEVEL SECURITY per migration 20260603000202 —
// inspection from test code requires the platform-scope bypass
// (`SELECT set_config('app.is_platform','true',false)`). These
// helpers internalise that bypass so callers never touch raw SQL
// or platform GUCs directly.
//
// Why this package exists: see `audittest` — the broader rationale
// is "tests get the same typed-helper discipline as production
// (sqlc + adapters); no raw SQL in test files".
package messagingtest

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	// pgx stdlib driver registered for the sql.Open("pgx", ...) bypass path.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Schema identifies the bounded context's outbox table. Each module
// owns its outbox per ADR 0001 (schema-per-module). The fixed enum
// constants below are the ONLY values that can flow into the helper
// queries' `%s.outbox` interpolation — gosec G201 is a false-positive
// here because the format string is a const + the only %s value is a
// closed-set enum.
type Schema string

// Schema constants — one per bounded context's outbox table.
const (
	SchemaIdentity  Schema = "identity"
	SchemaInventory Schema = "inventory"
	SchemaPlatform  Schema = "platform"
)

// OutboxCountByTenant returns the number of rows in <schema>.outbox
// matching the supplied tenant_id. Internally opens a raw DB
// connection from the pool's DSN, sets `app.is_platform=true` to
// bypass the outbox's FORCE RLS, runs the query, closes.
func OutboxCountByTenant(t testing.TB, pool *pgxpool.Pool, schema Schema, tenantID uuid.UUID) int64 {
	t.Helper()
	db := openBypassDB(t, pool)
	defer func() { _ = db.Close() }()
	var n int64
	q := fmt.Sprintf(`SELECT count(*) FROM %s.outbox WHERE tenant_id = $1`, schema) //nolint:gosec // G201: schema is a closed-set enum (Schema constants); not user-controlled
	if err := db.QueryRowContext(t.Context(), q, tenantID).Scan(&n); err != nil {
		t.Fatalf("messagingtest.OutboxCountByTenant(%s, %s): %v", schema, tenantID, err)
	}
	return n
}

// OutboxCountByTopic returns the number of rows in <schema>.outbox
// matching the supplied topic.
func OutboxCountByTopic(t testing.TB, pool *pgxpool.Pool, schema Schema, topic string) int64 {
	t.Helper()
	db := openBypassDB(t, pool)
	defer func() { _ = db.Close() }()
	var n int64
	q := fmt.Sprintf(`SELECT count(*) FROM %s.outbox WHERE topic = $1`, schema) //nolint:gosec // G201: schema is closed-set enum
	if err := db.QueryRowContext(t.Context(), q, topic).Scan(&n); err != nil {
		t.Fatalf("messagingtest.OutboxCountByTopic(%s, %q): %v", schema, topic, err)
	}
	return n
}

// OutboxTopicsForTenant returns the list of topics drained for the
// supplied tenant_id, ordered by occurred_at. Useful for asserting
// event-emission ORDER (e.g. "Created before Activated").
func OutboxTopicsForTenant(t testing.TB, pool *pgxpool.Pool, schema Schema, tenantID string) []string {
	t.Helper()
	db := openBypassDB(t, pool)
	defer func() { _ = db.Close() }()
	q := fmt.Sprintf(`SELECT topic FROM %s.outbox WHERE tenant_id = $1 ORDER BY occurred_at`, schema) //nolint:gosec // G201: schema is closed-set enum
	rows, err := db.QueryContext(t.Context(), q, tenantID)
	if err != nil {
		t.Fatalf("messagingtest.OutboxTopicsForTenant(%s, %s): %v", schema, tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var topics []string
	for rows.Next() {
		var topic string
		if err := rows.Scan(&topic); err != nil {
			t.Fatalf("scan: %v", err)
		}
		topics = append(topics, topic)
	}
	return topics
}

// OutboxFirstTopicForTopic returns the first topic+tenantID pair
// matching the supplied topic. Used by tests asserting "the latest
// emitted X event landed with the expected tenant scope".
func OutboxFirstTopicForTopic(t testing.TB, pool *pgxpool.Pool, schema Schema, topic string) (gotTopic string, gotTenant string) {
	t.Helper()
	db := openBypassDB(t, pool)
	defer func() { _ = db.Close() }()
	q := fmt.Sprintf(`SELECT topic, tenant_id FROM %s.outbox WHERE topic = $1 LIMIT 1`, schema) //nolint:gosec // G201: schema is closed-set enum
	if err := db.QueryRowContext(t.Context(), q, topic).Scan(&gotTopic, &gotTenant); err != nil {
		t.Fatalf("messagingtest.OutboxFirstTopicForTopic(%s, %q): %v", schema, topic, err)
	}
	return gotTopic, gotTenant
}

// OutboxLatestTopicAndTenantNull returns (topic, tenant_id_is_null)
// for the most recent row by created_at. Used by tests asserting
// that platform-scoped events (cross-tenant) carry tenant_id = NULL.
func OutboxLatestTopicAndTenantNull(t testing.TB, pool *pgxpool.Pool, schema Schema) (topic string, tenantIsNull bool) {
	t.Helper()
	db := openBypassDB(t, pool)
	defer func() { _ = db.Close() }()
	q := fmt.Sprintf(`SELECT topic, (tenant_id IS NULL) FROM %s.outbox ORDER BY created_at DESC LIMIT 1`, schema) //nolint:gosec // G201: schema is closed-set enum
	if err := db.QueryRowContext(t.Context(), q).Scan(&topic, &tenantIsNull); err != nil {
		t.Fatalf("messagingtest.OutboxLatestTopicAndTenantNull(%s): %v", schema, err)
	}
	return topic, tenantIsNull
}

// openBypassDB opens a sql.DB from the pool's DSN and sets the
// platform GUC for the connection's lifetime. Internal helper — not
// exported because callers should use the typed query helpers above.
func openBypassDB(t testing.TB, pool *pgxpool.Pool) *sql.DB {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := cfg.ConnString()
	if dsn == "" {
		// Fall back to building DSN from connection-config fields. Matches
		// the inventory fixture's openRawDB shape (Wave 6+ adapters use
		// this form when ConnString isn't preserved).
		dsn = fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("messagingtest.openBypassDB: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `SELECT set_config('app.is_platform','true',false)`); err != nil {
		_ = db.Close()
		t.Fatalf("messagingtest.openBypassDB: set platform GUC: %v", err)
	}
	return db
}
