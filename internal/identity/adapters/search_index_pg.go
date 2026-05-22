// search_index_pg.go — concrete pg-backed [query.SearchIndex].
//
// Lives in the adapters package (where the sqlc-generated db.* package
// is allowed to be imported) per ADR 0047 boundary discipline. The
// consumer-side interface [query.SearchIndex] is defined in
// internal/identity/app/query/search.go.

package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// SearchIndexPG implements [query.SearchIndex] over pg_trgm GIN
// indexes on identity.persons + identity.tenants (created by migration
// 20260518000001). Runs every query under [pg.TxScopePlatform] — the
// search surface is operator-only; HTTP boundary gates is_platform.
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

// Compile-time interface satisfaction.
var _ query.SearchIndex = (*SearchIndexPG)(nil)

// SearchPersons runs the pg_trgm similarity query against identity.persons.
// Returns ([], context.DeadlineExceeded) on timeout — the handler treats
// that as partial.
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
				ID:        uuidFromPg(r.ID).String(),
				Email:     r.Email,
				FirstName: r.FirstName,
				LastName:  r.LastName,
				CreatedAt: timeFromPg(r.CreatedAt),
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
				ID:          uuidFromPg(r.ID).String(),
				Slug:        r.Slug,
				LegalName:   r.LegalName,
				DisplayName: r.DisplayName,
				Status:      r.Status,
				CreatedAt:   timeFromPg(r.CreatedAt),
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search index: tenants: %w", err)
	}
	return hits, nil
}
