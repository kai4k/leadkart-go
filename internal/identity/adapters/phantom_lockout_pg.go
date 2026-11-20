package adapters

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// phantomKeyLabel domain-separates the derived key: the server secret signs JWTs
// too, and a key reused as-is across purposes is a different protocol per use.
const phantomKeyLabel = "leadkart/identity/login-phantom-failures/v1"

// PhantomLockoutRepository is the Postgres-backed command.PhantomFailureCounter: the
// failed-login counter for addresses with no usable account (unknown,
// deactivated, anonymised), with the same thresholds and the same atomic
// statement shape as a real account's counter (sql/phantom_lockout.sql next to
// RegisterPersonFailedLogin). Rows survive restarts and are shared by every
// replica, and concurrent attempts are serialised by the row lock exactly as for
// a real account.
//
// The key is HMAC-SHA-256(derived key, canonical email). The derived key is
// HMAC-SHA-256(serverSecret, label): an attacker who can read the table learns no
// address and cannot test a guess without the server secret, and no plain or
// unsalted hash of a non-user's email is ever stored.
type PhantomLockoutRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
	key  []byte
}

// NewPhantomLockoutRepository wires the repository. serverSecret must be a
// server-only secret (the JWT signing key in production wiring); it is never
// stored and rotating it just restarts the counters.
func NewPhantomLockoutRepository(pool *pgxpool.Pool, tx *pg.Transactor, serverSecret []byte) *PhantomLockoutRepository {
	if len(serverSecret) == 0 {
		panic("adapters: NewPhantomLockoutRepository serverSecret required")
	}
	mac := hmac.New(sha256.New, serverSecret)
	mac.Write([]byte(phantomKeyLabel))
	return &PhantomLockoutRepository{pool: pool, tx: tx, q: db.New(pool), key: mac.Sum(nil)}
}

// rowKey is the table key of an address: the HMAC of its canonical form.
func (r *PhantomLockoutRepository) rowKey(e email.Address) []byte {
	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(e.String()))))
	return mac.Sum(nil)
}

// RegisterFailure implements command.PhantomFailureCounter: one atomic upsert counts the
// attempt and applies the lock policy, and the answer is read from the row it
// wrote.
func (r *PhantomLockoutRepository) RegisterFailure(ctx context.Context, e email.Address, now time.Time) (person.LockoutOutcome, error) {
	var out person.LockoutOutcome
	err := r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, err := r.q.WithTx(tx).RegisterPhantomLoginFailure(ctx, db.RegisterPhantomLoginFailureParams{
			Key:        r.rowKey(e),
			Now:        pgconv.PgRequiredTimestamp(now),
			WindowSecs: person.LockoutWindow.Seconds(),
			MaxFailed:  person.MaxFailedLogins,
			LockSecs:   person.LockoutDuration.Seconds(),
		})
		if err != nil {
			return fmt.Errorf("phantom lockout: register: %w", err)
		}
		out = person.LockoutOutcome{
			FailedCount: int(row.FailedCount),
			LockedUntil: pgconv.TimeFromPg(row.LockedUntil),
		}
		return nil
	})
	return out, err
}

// PurgeExpired deletes up to batch rows whose window and lock have both passed
// and returns how many. The purge job calls it until a batch comes back short.
func (r *PhantomLockoutRepository) PurgeExpired(ctx context.Context, now time.Time, batch int) (int64, error) {
	var n int64
	err := r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		got, err := r.q.WithTx(tx).PurgePhantomLoginFailures(ctx, db.PurgePhantomLoginFailuresParams{
			WindowCutoff: pgconv.PgRequiredTimestamp(now.Add(-person.LockoutWindow)),
			Now:          pgconv.PgRequiredTimestamp(now),
			BatchSize:    int32(batch), //nolint:gosec // bounded by the caller's constant
		})
		if err != nil {
			return fmt.Errorf("phantom lockout: purge: %w", err)
		}
		n = got
		return nil
	})
	return n, err
}

func (r *PhantomLockoutRepository) runInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, fn)
}
