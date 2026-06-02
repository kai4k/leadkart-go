// search_index_pg.go — pg-backed [query.SearchIndex] (ADR 0047).
// Consumer-side interface: internal/identity/app/query/search.go.

package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
)

// SearchIndexPG implements [query.SearchIndex] over pg_trgm GIN indexes on
// identity.persons and identity.tenants (migration 20260518000001).
// Queries run under [pg.TxScopePlatform]; HTTP boundary gates is_platform.
type SearchIndexPG struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewSearchIndexPG wires the adapter.
func NewSearchIndexPG(pool *pgxpool.Pool, tx *pg.Transactor) *SearchIndexPG {
	if pool == nil {
		panic("adapters: NewSearchIndexPG pool required")
	}
	if tx == nil {
		panic("adapters: NewSearchIndexPG transactor required")
	}
	return &SearchIndexPG{pool: pool, tx: tx, q: db.New(pool)}
}

var _ query.SearchIndex = (*SearchIndexPG)(nil)

// SearchPersons runs the pg_trgm similarity query against identity.persons.
// Returns an empty slice + context.DeadlineExceeded on timeout.
func (s *SearchIndexPG) SearchPersons(ctx context.Context, q string, limit int32) ([]query.SearchPersonHit, error) {
	var hits []query.SearchPersonHit
	err := s.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.q.WithTx(tx).SearchPersonsByText(ctx, db.SearchPersonsByTextParams{
			Query: q,
			Limit: limit,
		})
		if err != nil {
			return err
		}
		hits = make([]query.SearchPersonHit, 0, len(rows))
		for _, r := range rows {
			hits = append(hits, query.SearchPersonHit{
				ID:        pgconv.UUIDFromPg(r.ID).String(),
				Email:     r.Email,
				FirstName: r.FirstName,
				LastName:  r.LastName,
				CreatedAt: pgconv.TimeFromPg(r.CreatedAt),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search index: persons: %w", err)
	}
	return hits, nil
}

// SearchTenants runs the pg_trgm similarity query against identity.tenants.
func (s *SearchIndexPG) SearchTenants(ctx context.Context, q string, limit int32) ([]query.SearchTenantHit, error) {
	var hits []query.SearchTenantHit
	err := s.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.q.WithTx(tx).SearchTenantsByText(ctx, db.SearchTenantsByTextParams{
			Query: q,
			Limit: limit,
		})
		if err != nil {
			return err
		}
		hits = make([]query.SearchTenantHit, 0, len(rows))
		for _, r := range rows {
			hits = append(hits, query.SearchTenantHit{
				ID:          pgconv.UUIDFromPg(r.ID).String(),
				Slug:        r.Slug,
				LegalName:   r.LegalName,
				DisplayName: r.DisplayName,
				Status:      r.Status,
				CreatedAt:   pgconv.TimeFromPg(r.CreatedAt),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search index: tenants: %w", err)
	}
	return hits, nil
}
