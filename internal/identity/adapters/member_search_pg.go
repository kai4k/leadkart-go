// member_search_pg.go — pg-backed [query.MemberSearcher].

package adapters

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// MemberSearchPG implements [query.MemberSearcher] under the TENANT scope (TxScopeTenant:
// WithinTxPgxTenant binds the tenant GUC), RLS on identity.tenant_memberships, and the tenant is bound again in the SQL.
type MemberSearchPG struct {
	tx *pg.Transactor
	q  *db.Queries
}

// NewMemberSearchPG wires the adapter.
func NewMemberSearchPG(pool *pgxpool.Pool, tx *pg.Transactor) *MemberSearchPG {
	if pool == nil {
		panic("adapters: NewMemberSearchPG pool required")
	}
	if tx == nil {
		panic("adapters: NewMemberSearchPG transactor required")
	}
	return &MemberSearchPG{tx: tx, q: db.New(pool)}
}

var _ query.MemberSearcher = (*MemberSearchPG)(nil)

// SearchActiveMembers satisfies [query.MemberSearcher].
func (s *MemberSearchPG) SearchActiveMembers(ctx context.Context, tenantID tenant.ID, text string, limit int) ([]query.MemberHit, error) {
	// LIKE metacharacters are escaped so a typed "%" or "_" matches itself.
	likeEscaper := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return nil, fmt.Errorf("member search: tenant id: %w", err)
	}
	var hits []query.MemberHit
	err = s.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, qerr := s.q.WithTx(tx).SearchActiveMembersByName(ctx, db.SearchActiveMembersByNameParams{
			TenantID: pgconv.PgUUID(tid),
			Pattern:  "%" + likeEscaper.Replace(text) + "%",
			Limit:    int32(limit), //nolint:gosec // the handler clamps limit to 20
		})
		if qerr != nil {
			return qerr
		}
		hits = make([]query.MemberHit, 0, len(rows))
		for _, r := range rows {
			hits = append(hits, query.MemberHit{
				MembershipID: pgconv.UUIDFromPg(r.MembershipID).String(),
				DisplayName:  r.FirstName + " " + r.LastName,
				Designation:  r.Designation,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("member search: %w", err)
	}
	return hits, nil
}
