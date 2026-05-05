package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// RefreshTokenFamilyRepository is the pgx/sqlc-backed implementation of
// [refreshtoken.Repository]. The two underlying tables
// (refresh_token_families + refresh_tokens) are NON-RLS — refresh tokens
// are session-management infrastructure that carries tenant context as a
// data column. Token-hash uniqueness is the load-bearing isolation per
// Auth0/Okta/Stripe canon; cross-tenant lookups by token hash are
// intentional + the foundational read pattern.
//
// All paths run under TxScopePlatform: the outbox row's tenant_id is
// the family's tenantID and the policy `is_platform()` always permits.
type RefreshTokenFamilyRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *Queries
}

// NewRefreshTokenFamilyRepository wires the repository.
func NewRefreshTokenFamilyRepository(pool *pgxpool.Pool, tx *pg.Transactor) *RefreshTokenFamilyRepository {
	return &RefreshTokenFamilyRepository{pool: pool, tx: tx, q: New(pool)}
}

// Add persists a brand-new family + its first token from [refreshtoken.NewFamily].
func (r *RefreshTokenFamilyRepository) Add(ctx context.Context, f *refreshtoken.Family) error {
	return r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
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

// UpdateByID — TDL UpdateFn pattern. Loads family + tokens, runs
// closure, persists whatever the aggregate now says. The UPSERT-by-id
// path covers both the "new token minted" case (insert) and the
// "previous token consumed" case (update consumed_at + replaced_by_id)
// without explicit delta tracking.
func (r *RefreshTokenFamilyRepository) UpdateByID(
	ctx context.Context,
	id refreshtoken.FamilyID,
	updateFn func(*refreshtoken.Family) (bool, error),
) error {
	return r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
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

// GetByID returns the family + all its tokens, or [refreshtoken.ErrNotFound].
func (r *RefreshTokenFamilyRepository) GetByID(ctx context.Context, id refreshtoken.FamilyID) (*refreshtoken.Family, error) {
	var out *refreshtoken.Family
	err := r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
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

// GetByTokenHash resolves a presented refresh token to its family.
// Returns [refreshtoken.ErrNotFound] when no family has a token with
// the given hash.
func (r *RefreshTokenFamilyRepository) GetByTokenHash(ctx context.Context, hash refreshtoken.TokenHash) (*refreshtoken.Family, error) {
	var out *refreshtoken.Family
	err := r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
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

// ListActiveForPerson returns all non-revoked families for a Person —
// powers the user "manage sessions" UI + the per-Person family-cap
// enforcement at family creation time.
func (r *RefreshTokenFamilyRepository) ListActiveForPerson(ctx context.Context, personID person.ID) ([]*refreshtoken.Family, error) {
	uid, err := parsePersonIDForRefresh(personID)
	if err != nil {
		return nil, err
	}
	var out []*refreshtoken.Family
	err = r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		// Two-pass: drain family rows into a slice (releasing the
		// connection's iteration cursor), THEN load each family's
		// tokens. pgx forbids multiplexed queries on one connection.
		rows, err := tx.Query(ctx, `
			SELECT id, person_id, tenant_id, device_label,
			       created_at, last_used_at, revoked_at, revoke_reason
			FROM   identity.refresh_token_families
			WHERE  person_id = $1
			  AND  revoked_at IS NULL
			ORDER  BY created_at
		`, pgUUID(uid))
		if err != nil {
			return fmt.Errorf("refresh token repo: list active: %w", err)
		}
		var familyRows []IdentityRefreshTokenFamily
		for rows.Next() {
			var row IdentityRefreshTokenFamily
			if err := rows.Scan(
				&row.ID, &row.PersonID, &row.TenantID, &row.DeviceLabel,
				&row.CreatedAt, &row.LastUsedAt, &row.RevokedAt, &row.RevokeReason,
			); err != nil {
				rows.Close()
				return fmt.Errorf("refresh token repo: scan: %w", err)
			}
			familyRows = append(familyRows, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		q := r.q.WithTx(tx)
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

func insertFamilyRow(ctx context.Context, q *Queries, f *refreshtoken.Family) error {
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
	err = q.InsertRefreshTokenFamily(ctx, InsertRefreshTokenFamilyParams{
		ID:           pgUUID(uid),
		PersonID:     pgUUID(pid),
		TenantID:     pgUUID(tid),
		DeviceLabel:  f.DeviceLabel(),
		CreatedAt:    pgRequiredTimestamp(f.CreatedAt()),
		LastUsedAt:   pgRequiredTimestamp(f.LastUsedAt()),
		RevokedAt:    pgTimestamp(f.RevokedAt()),
		RevokeReason: nilIfEmpty(f.RevokeReason()),
	})
	if err != nil {
		return fmt.Errorf("refresh token repo: insert family: %w", err)
	}
	return nil
}

func persistFamilyState(ctx context.Context, q *Queries, f *refreshtoken.Family) error {
	uid, err := parseFamilyID(f.ID())
	if err != nil {
		return err
	}
	err = q.UpdateRefreshTokenFamily(ctx, UpdateRefreshTokenFamilyParams{
		ID:           pgUUID(uid),
		LastUsedAt:   pgRequiredTimestamp(f.LastUsedAt()),
		RevokedAt:    pgTimestamp(f.RevokedAt()),
		RevokeReason: nilIfEmpty(f.RevokeReason()),
	})
	if err != nil {
		return fmt.Errorf("refresh token repo: update family: %w", err)
	}
	return nil
}

func upsertFamilyTokens(ctx context.Context, q *Queries, f *refreshtoken.Family) error {
	fid, err := parseFamilyID(f.ID())
	if err != nil {
		return err
	}
	// Iterate in REVERSE generation order so that when an older token's
	// replaced_by_id points at a newer token, the newer one already
	// exists (FK refresh_tokens_replaced_by_id_fkey is satisfied at
	// statement time — Postgres FK checks run per-statement under
	// READ COMMITTED).
	tokens := f.AllTokens()
	for i := len(tokens) - 1; i >= 0; i-- {
		t := tokens[i]
		tid, err := parseTokenID(t.ID())
		if err != nil {
			return err
		}
		var replacedBy = pgUUIDOpt(uuid.Nil)
		if !t.ReplacedByID().IsZero() {
			rb, err := parseTokenID(t.ReplacedByID())
			if err != nil {
				return err
			}
			replacedBy = pgUUIDOpt(rb)
		}
		err = q.UpsertRefreshToken(ctx, UpsertRefreshTokenParams{
			ID:           pgUUID(tid),
			FamilyID:     pgUUID(fid),
			TokenHash:    t.Hash().String(),
			Generation:   t.Generation(),
			IssuedAt:     pgRequiredTimestamp(t.IssuedAt()),
			ExpiresAt:    pgRequiredTimestamp(t.ExpiresAt()),
			ConsumedAt:   pgTimestamp(t.ConsumedAt()),
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
	out := make([]outboxEvent, len(evs))
	for i, e := range evs {
		out[i] = e
	}
	return writeOutboxEvents(ctx, tx, tid, out)
}

func loadFamilyByID(ctx context.Context, q *Queries, id refreshtoken.FamilyID) (*refreshtoken.Family, error) {
	uid, err := parseFamilyID(id)
	if err != nil {
		return nil, err
	}
	row, err := q.GetRefreshTokenFamilyByID(ctx, pgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refreshtoken.ErrNotFound
		}
		return nil, fmt.Errorf("refresh token repo: get family: %w", err)
	}
	return hydrateFamily(ctx, q, row)
}

func hydrateFamily(ctx context.Context, q *Queries, row IdentityRefreshTokenFamily) (*refreshtoken.Family, error) {
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
			replacedBy = refreshtoken.TokenID(uuidFromPg(t.ReplacedByID).String())
		}
		tokenSnaps[i] = refreshtoken.TokenSnapshot{
			ID:           refreshtoken.TokenID(uuidFromPg(t.ID).String()),
			Hash:         hash,
			Generation:   t.Generation,
			IssuedAt:     timeFromPg(t.IssuedAt),
			ExpiresAt:    timeFromPg(t.ExpiresAt),
			ConsumedAt:   timeFromPg(t.ConsumedAt),
			ReplacedByID: replacedBy,
		}
	}
	var revokeReason string
	if row.RevokeReason != nil {
		revokeReason = *row.RevokeReason
	}
	return refreshtoken.UnmarshalFromDB(refreshtoken.FamilySnapshot{
		ID:           refreshtoken.FamilyID(uuidFromPg(row.ID).String()),
		PersonID:     person.ID(uuidFromPg(row.PersonID).String()),
		TenantID:     tenant.ID(uuidFromPg(row.TenantID).String()),
		DeviceLabel:  row.DeviceLabel,
		CreatedAt:    timeFromPg(row.CreatedAt),
		LastUsedAt:   timeFromPg(row.LastUsedAt),
		RevokedAt:    timeFromPg(row.RevokedAt),
		RevokeReason: revokeReason,
		Tokens:       tokenSnaps,
	}), nil
}

// nilIfEmpty maps "" → nil so sqlc-generated *string fields write SQL
// NULL rather than an empty-string row. Domain represents "no reason"
// as the zero string; the column is nullable.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
