package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RefreshTokenFamilyRepository is the pgx/sqlc-backed [refreshtoken.Repository].
// The underlying tables are non-RLS (session infrastructure); tenant context
// is a data column. All paths run under TxScopePlatform.
type RefreshTokenFamilyRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewRefreshTokenFamilyRepository wires the repository.
func NewRefreshTokenFamilyRepository(pool *pgxpool.Pool, tx *pg.Transactor) *RefreshTokenFamilyRepository {
	return &RefreshTokenFamilyRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// runInTx joins an ambient UoW tx when present, else opens a platform-scoped
// tx (ADR 0067 Phase-4 UoW-join contract). Previously calling WithinTxPgx
// directly caused inbox-wrapped revocations to split into separate
// transactions.
func (r *RefreshTokenFamilyRepository) runInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, fn)
}

// Add persists a new family and its first token.
func (r *RefreshTokenFamilyRepository) Add(ctx context.Context, f *refreshtoken.Family) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if err := insertFamilyRow(ctx, q, f); err != nil {
			return err
		}
		if err := upsertFamilyTokens(ctx, q, f); err != nil {
			return err
		}
		return drainFamilyEvents(ctx, tx, f)
	})
}

// UpdateByID uses the TDL UpdateFn pattern. UPSERT-by-id covers both
// new-token-mint (insert) and consumed-token (update) without delta tracking.
func (r *RefreshTokenFamilyRepository) UpdateByID(
	ctx context.Context,
	id refreshtoken.FamilyID,
	updateFn func(*refreshtoken.Family) (bool, error),
) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		f, err := loadFamilyByID(ctx, q, id)
		if err != nil {
			return err
		}
		shouldPersist, err := updateFn(f)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := persistFamilyState(ctx, q, f); err != nil {
			return err
		}
		if err := upsertFamilyTokens(ctx, q, f); err != nil {
			return err
		}
		return drainFamilyEvents(ctx, tx, f)
	})
}

// GetByID returns the family and all its tokens, or [refreshtoken.ErrNotFound].
func (r *RefreshTokenFamilyRepository) GetByID(ctx context.Context, id refreshtoken.FamilyID) (*refreshtoken.Family, error) {
	var out *refreshtoken.Family
	err := r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		f, err := loadFamilyByID(ctx, r.q.WithTx(tx), id)
		if err != nil {
			return err
		}
		out = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetByTokenHash resolves a token hash to its family, or returns
// [refreshtoken.ErrNotFound].
func (r *RefreshTokenFamilyRepository) GetByTokenHash(ctx context.Context, hash refreshtoken.TokenHash) (*refreshtoken.Family, error) {
	var out *refreshtoken.Family
	err := r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetRefreshTokenByHash(ctx, hash.String())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return refreshtoken.ErrNotFound
			}
			return fmt.Errorf("refresh token repo: get by hash: %w", err)
		}
		familyRow, err := q.GetRefreshTokenFamilyByID(ctx, row.FamilyID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return refreshtoken.ErrNotFound
			}
			return fmt.Errorf("refresh token repo: get family by hash row: %w", err)
		}
		f, err := hydrateFamily(ctx, q, familyRow)
		if err != nil {
			return err
		}
		out = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListActiveForPerson returns all non-revoked families for a Person.
// Powers the "manage sessions" UI and per-Person family-cap enforcement.
func (r *RefreshTokenFamilyRepository) ListActiveForPerson(ctx context.Context, personID person.ID) ([]*refreshtoken.Family, error) {
	uid, err := parsePersonIDForRefresh(personID)
	if err != nil {
		return nil, err
	}
	var out []*refreshtoken.Family
	err = r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Two-pass: collect family rows, then load tokens per family.
		// pgx forbids multiplexed queries on one connection.
		q := r.q.WithTx(tx)
		familyRows, err := q.ListActiveFamiliesForPerson(ctx, pgconv.PgUUID(uid))
		if err != nil {
			return fmt.Errorf("refresh token repo: list active: %w", err)
		}
		for _, row := range familyRows {
			f, herr := hydrateFamily(ctx, q, row)
			if herr != nil {
				return herr
			}
			out = append(out, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ----- Helpers ---------------------------------------------------------------

func insertFamilyRow(ctx context.Context, q *db.Queries, f *refreshtoken.Family) error {
	uid, err := parseFamilyID(f.ID())
	if err != nil {
		return err
	}
	pid, err := parsePersonIDForRefresh(f.PersonID())
	if err != nil {
		return err
	}
	tid, err := parseTenantIDForRefresh(f.TenantID())
	if err != nil {
		return err
	}
	err = q.InsertRefreshTokenFamily(ctx, db.InsertRefreshTokenFamilyParams{
		ID:           pgconv.PgUUID(uid),
		PersonID:     pgconv.PgUUID(pid),
		TenantID:     pgconv.PgUUID(tid),
		DeviceLabel:  f.DeviceLabel(),
		CreatedAt:    pgconv.PgRequiredTimestamp(f.CreatedAt()),
		LastUsedAt:   pgconv.PgRequiredTimestamp(f.LastUsedAt()),
		RevokedAt:    pgconv.PgTimestamp(f.RevokedAt()),
		RevokeReason: pgconv.ZeroToNil(f.RevokeReason()),
	})
	if err != nil {
		return fmt.Errorf("refresh token repo: insert family: %w", err)
	}
	return nil
}

func persistFamilyState(ctx context.Context, q *db.Queries, f *refreshtoken.Family) error {
	uid, err := parseFamilyID(f.ID())
	if err != nil {
		return err
	}
	err = q.UpdateRefreshTokenFamily(ctx, db.UpdateRefreshTokenFamilyParams{
		ID:           pgconv.PgUUID(uid),
		LastUsedAt:   pgconv.PgRequiredTimestamp(f.LastUsedAt()),
		RevokedAt:    pgconv.PgTimestamp(f.RevokedAt()),
		RevokeReason: pgconv.ZeroToNil(f.RevokeReason()),
	})
	if err != nil {
		return fmt.Errorf("refresh token repo: update family: %w", err)
	}
	return nil
}

func upsertFamilyTokens(ctx context.Context, q *db.Queries, f *refreshtoken.Family) error {
	fid, err := parseFamilyID(f.ID())
	if err != nil {
		return err
	}
	// Reverse generation order: newer tokens exist first so the
	// replaced_by_id FK (refresh_tokens_replaced_by_id_fkey) is
	// satisfied at statement time under READ COMMITTED.
	tokens := f.AllTokens()
	for i := len(tokens) - 1; i >= 0; i-- {
		t := tokens[i]
		tid, err := parseTokenID(t.ID())
		if err != nil {
			return err
		}
		var replacedBy = pgconv.PgUUIDOrNull(uuid.Nil)
		if !t.ReplacedByID().IsZero() {
			rb, err := parseTokenID(t.ReplacedByID())
			if err != nil {
				return err
			}
			replacedBy = pgconv.PgUUIDOrNull(rb)
		}
		err = q.UpsertRefreshToken(ctx, db.UpsertRefreshTokenParams{
			ID:           pgconv.PgUUID(tid),
			FamilyID:     pgconv.PgUUID(fid),
			TokenHash:    t.Hash().String(),
			Generation:   t.Generation(),
			IssuedAt:     pgconv.PgRequiredTimestamp(t.IssuedAt()),
			ExpiresAt:    pgconv.PgRequiredTimestamp(t.ExpiresAt()),
			ConsumedAt:   pgconv.PgTimestamp(t.ConsumedAt()),
			ReplacedByID: replacedBy,
		})
		if err != nil {
			return fmt.Errorf("refresh token repo: upsert token: %w", err)
		}
	}
	return nil
}

func drainFamilyEvents(ctx context.Context, tx pgx.Tx, f *refreshtoken.Family) error {
	evs := f.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := parseTenantIDForRefresh(f.TenantID())
	if err != nil {
		return err
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("refresh token repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

func loadFamilyByID(ctx context.Context, q *db.Queries, id refreshtoken.FamilyID) (*refreshtoken.Family, error) {
	uid, err := parseFamilyID(id)
	if err != nil {
		return nil, err
	}
	row, err := q.GetRefreshTokenFamilyByID(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refreshtoken.ErrNotFound
		}
		return nil, fmt.Errorf("refresh token repo: get family: %w", err)
	}
	return hydrateFamily(ctx, q, row)
}

func hydrateFamily(ctx context.Context, q *db.Queries, row db.IdentityRefreshTokenFamily) (*refreshtoken.Family, error) {
	tokens, err := q.ListRefreshTokensInFamily(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("refresh token repo: list tokens: %w", err)
	}
	tokenSnaps := make([]refreshtoken.TokenSnapshot, len(tokens))
	for i, t := range tokens {
		hash, herr := refreshtoken.NewTokenHash(t.TokenHash)
		if herr != nil {
			return nil, fmt.Errorf("refresh token repo: hydrate hash: %w", herr)
		}
		var replacedBy refreshtoken.TokenID
		if t.ReplacedByID.Valid {
			replacedBy = refreshtoken.TokenID(pgconv.UUIDFromPg(t.ReplacedByID).String())
		}
		tokenSnaps[i] = refreshtoken.TokenSnapshot{
			ID:           refreshtoken.TokenID(pgconv.UUIDFromPg(t.ID).String()),
			Hash:         hash,
			Generation:   t.Generation,
			IssuedAt:     pgconv.TimeFromPg(t.IssuedAt),
			ExpiresAt:    pgconv.TimeFromPg(t.ExpiresAt),
			ConsumedAt:   pgconv.TimeFromPg(t.ConsumedAt),
			ReplacedByID: replacedBy,
		}
	}
	var revokeReason string
	if row.RevokeReason != nil {
		revokeReason = *row.RevokeReason
	}
	return refreshtoken.UnmarshalFromDB(refreshtoken.FamilySnapshot{
		ID:           refreshtoken.FamilyID(pgconv.UUIDFromPg(row.ID).String()),
		PersonID:     person.ID(pgconv.UUIDFromPg(row.PersonID).String()),
		TenantID:     tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		DeviceLabel:  row.DeviceLabel,
		CreatedAt:    pgconv.TimeFromPg(row.CreatedAt),
		LastUsedAt:   pgconv.TimeFromPg(row.LastUsedAt),
		RevokedAt:    pgconv.TimeFromPg(row.RevokedAt),
		RevokeReason: revokeReason,
		Tokens:       tokenSnaps,
	}), nil
}

func parseFamilyID(id refreshtoken.FamilyID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("refresh token repo: parse family id %q: %w", id, err)
	}
	return parsed, nil
}

func parseTokenID(id refreshtoken.TokenID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("refresh token repo: parse token id %q: %w", id, err)
	}
	return parsed, nil
}

func parsePersonIDForRefresh(id person.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("refresh token repo: parse person id %q: %w", id, err)
	}
	return parsed, nil
}

func parseTenantIDForRefresh(id tenant.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("refresh token repo: parse tenant id %q: %w", id, err)
	}
	return parsed, nil
}
