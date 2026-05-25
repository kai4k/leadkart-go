package messagingtest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InboxCountForMessage returns the number of identity.processed_messages
// rows for the supplied (message_id, handler_name) pair. Used by
// idempotent-receiver tests to assert the dedup gate fired exactly
// once even after a redelivery.
func InboxCountForMessage(t testing.TB, pool *pgxpool.Pool, messageID string, handler string) int64 {
	t.Helper()
	var n int64
	const q = `
		SELECT count(*) FROM identity.processed_messages
		WHERE  message_id = $1 AND handler_name = $2
	`
	if err := pool.QueryRow(t.Context(), q, messageID, handler).Scan(&n); err != nil {
		t.Fatalf("messagingtest.InboxCountForMessage(%s, %s): %v", messageID, handler, err)
	}
	return n
}
