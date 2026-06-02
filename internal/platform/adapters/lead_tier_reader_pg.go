package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
)

// ErrTierNotFound is returned when a tier code has no platform.lead_tiers row
// — a config/deploy bug (the three tiers are seeded by migration).
var ErrTierNotFound = errors.New("platform tier reader: tier not found")

// LeadTierReader reads per-tier marketplace config from platform.lead_tiers.
// Implements [command.TierReader].
type LeadTierReader struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// NewLeadTierReader wires the reader.
func NewLeadTierReader(pool *pgxpool.Pool) *LeadTierReader {
	return &LeadTierReader{pool: pool, q: db.New(pool)}
}

// GetTier resolves the tier's default sale limit + base price. Runs on the
// active tx when one is present (the purchase path) so the read is part of the
// same transaction.
func (r *LeadTierReader) GetTier(ctx context.Context, tier platformlead.Tier) (platformlead.TierConfig, error) {
	q := r.q
	if tx, ok := pg.TxFromContext(ctx); ok {
		q = r.q.WithTx(tx)
	}
	row, err := q.GetLeadTier(ctx, tier.String())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return platformlead.TierConfig{}, fmt.Errorf("%w: %q", ErrTierNotFound, tier)
		}
		return platformlead.TierConfig{}, fmt.Errorf("platform tier reader: get %q: %w", tier, err)
	}
	return platformlead.TierConfig{
		Code:             platformlead.Tier(row.Code),
		DefaultSaleLimit: int(row.DefaultSaleLimit),
		BasePricePaisa:   row.BasePricePaisa,
	}, nil
}
