package pg

// PostgreSQL SQLSTATE codes used in repository / handler error
// classification. Mirror of `lib/pq.ErrorCode` style — pgx's
// `*pgconn.PgError.Code` field carries the same SQLSTATE strings.
//
// Per `coding-standards.md` "No magic strings — production AND tests":
// every SQLSTATE used in `errors.As` / equality branches is declared
// here once and referenced by name everywhere else.
//
// Reference: https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	// SQLStateUniqueViolation — Class 23 (Integrity Constraint Violation),
	// "unique_violation" 23505. Repository layer maps this to a domain
	// `ErrAlreadyExists` / `ErrConflict` per the table involved.
	SQLStateUniqueViolation = "23505"

	// SQLStateForeignKeyViolation — Class 23, "foreign_key_violation" 23503.
	// Repository surfaces this as `ErrParentNotFound` for inserts whose FK
	// target row was deleted concurrently.
	SQLStateForeignKeyViolation = "23503"

	// SQLStateCheckViolation — Class 23, "check_violation" 23514. Surfaces
	// when a CHECK constraint (e.g. enum-like text columns, length bounds)
	// rejects the row. Repository should map to a domain validation error.
	SQLStateCheckViolation = "23514"

	// SQLStateNotNullViolation — Class 23, "not_null_violation" 23502.
	// Indicates a programmer error (missing column in INSERT) — repository
	// surfaces this as a generic 500 since it's never user-facing.
	SQLStateNotNullViolation = "23502"

	// SQLStateSerializationFailure — Class 40 (Transaction Rollback),
	// "serialization_failure" 40001. Occurs under SERIALIZABLE / REPEATABLE
	// READ isolation when a concurrent transaction conflict is detected.
	// Caller should retry the transaction.
	SQLStateSerializationFailure = "40001"

	// SQLStateUndefinedFunction — Class 42 (Syntax Error or Access Rule
	// Violation), "undefined_function" 42883. Common signal of Marten /
	// pgx codegen drift (e.g. `mt_upsert_*` function missing because
	// schema bootstrap didn't run). Always a programmer / deploy bug.
	SQLStateUndefinedFunction = "42883"
)
