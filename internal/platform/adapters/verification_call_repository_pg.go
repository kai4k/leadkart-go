package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

// VerificationCallRepository is the pgx/sqlc implementation of
// [verificationcall.Repository]. Append-only; no UpdateByID.
type VerificationCallRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewVerificationCallRepository wires the repository.
func NewVerificationCallRepository(pool *pgxpool.Pool, tx *pg.Transactor) *VerificationCallRepository {
	return &VerificationCallRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [verificationcall.Repository].
func (r *VerificationCallRepository) Add(ctx context.Context, c *verificationcall.VerificationCall) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, c)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, c)
	})
}

func (r *VerificationCallRepository) addOnTx(ctx context.Context, tx pgx.Tx, c *verificationcall.VerificationCall) error {
	q := r.q.WithTx(tx)
	if err := insertVerificationCallRow(ctx, q, c); err != nil {
		return err
	}
	// Platform-scoped: tenant_id = uuid.Nil.
	evs := c.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	return drainEventsToOutbox(ctx, tx, uuid.Nil, asAny)
}

// ListByContact satisfies [verificationcall.Repository].
func (r *VerificationCallRepository) ListByContact(
	ctx context.Context, contactID unverifiedcontact.ID,
) ([]*verificationcall.VerificationCall, error) {
	cid, err := uuid.Parse(contactID.String())
	if err != nil {
		return nil, fmt.Errorf("verification call repo: parse contact id: %w", err)
	}
	q := r.q
	if tx, ok := pg.TxFromContext(ctx); ok {
		q = r.q.WithTx(tx)
	}
	rows, err := q.ListVerificationCallsByContact(ctx, pgconv.PgUUID(cid))
	if err != nil {
		return nil, fmt.Errorf("verification call repo: list: %w", err)
	}
	out := make([]*verificationcall.VerificationCall, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToVerificationCall(row))
	}
	return out, nil
}

func insertVerificationCallRow(ctx context.Context, q *db.Queries, c *verificationcall.VerificationCall) error {
	cid, err := uuid.Parse(c.ID().String())
	if err != nil {
		return fmt.Errorf("verification call repo: parse id: %w", err)
	}
	contactID, err := uuid.Parse(c.ContactID().String())
	if err != nil {
		return fmt.Errorf("verification call repo: parse contact id: %w", err)
	}
	loggedBy, err := uuid.Parse(c.LoggedByMembershipID().String())
	if err != nil {
		return fmt.Errorf("verification call repo: parse logged_by: %w", err)
	}
	err = q.InsertVerificationCall(ctx, db.InsertVerificationCallParams{
		ID:                    pgconv.PgUUID(cid),
		ContactID:             pgconv.PgUUID(contactID),
		OutcomeCode:           string(c.Outcome()),
		Notes:                 c.Notes(),
		CallbackWindowStartAt: pgconv.PgTimestamp(c.CallbackWindowStartAt()),
		CallbackWindowEndAt:   pgconv.PgTimestamp(c.CallbackWindowEndAt()),
		LoggedAt:              pgconv.PgRequiredTimestamp(c.LoggedAt()),
		LoggedByMembershipID:  pgconv.PgUUID(loggedBy),
	})
	if err != nil {
		return fmt.Errorf("verification call repo: insert: %w", err)
	}
	return nil
}

func rowToVerificationCall(row db.PlatformVerificationCall) *verificationcall.VerificationCall {
	return verificationcall.UnmarshalFromDB(verificationcall.Snapshot{
		ID:                    verificationcall.ID(pgconv.UUIDFromPg(row.ID).String()),
		ContactID:             unverifiedcontact.ID(pgconv.UUIDFromPg(row.ContactID).String()),
		Outcome:               verificationcall.OutcomeCode(row.OutcomeCode),
		Notes:                 row.Notes,
		CallbackWindowStartAt: pgconv.TimeFromPg(row.CallbackWindowStartAt),
		CallbackWindowEndAt:   pgconv.TimeFromPg(row.CallbackWindowEndAt),
		LoggedAt:              pgconv.TimeFromPg(row.LoggedAt),
		LoggedByMembershipID:  unverifiedcontact.MembershipID(pgconv.UUIDFromPg(row.LoggedByMembershipID).String()),
	})
}
