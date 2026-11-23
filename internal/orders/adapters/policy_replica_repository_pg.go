package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters/db"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/app/query"
)

// ErrPolicyReplicaNotFound is the internal "no row" answer of the replica read;
// ReadPolicy turns it into commercepolicy.Initial().
var ErrPolicyReplicaNotFound = errors.New("orders policy replica: not found")

// PolicyReplicaRepository is the pgx/sqlc-backed replica of the company commerce
// policy: it implements [command.PolicyReplicaStore] (written by the
// companysettings event subscriber) and [query.PolicyReader] (read by Orders
// business code). Tenant-scoped (TxScopeTenant semantics: the GUC is bound from
// the explicit tenant id); every method joins the transaction on ctx.
type PolicyReplicaRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewPolicyReplicaRepository wires the repository.
func NewPolicyReplicaRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PolicyReplicaRepository {
	return &PolicyReplicaRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var (
	_ command.PolicyReplicaStore = (*PolicyReplicaRepository)(nil)
	_ query.PolicyReader         = (*PolicyReplicaRepository)(nil)
)

// runInTx joins the surrounding UoW tx when ctx carries one (a second pooled
// connection taken while the caller holds one deadlocks the pool), else runs fn
// in a fresh tenant-scoped tx.
func (r *PolicyReplicaRepository) runInTx(ctx context.Context, tenantID tenant.ID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), fn)
}

// StoreIfNewer satisfies [command.PolicyReplicaStore].
func (r *PolicyReplicaRepository) StoreIfNewer(ctx context.Context, tenantID tenant.ID, policy commercepolicy.Versioned) (bool, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return false, fmt.Errorf("orders policy replica: parse tenant id: %w", err)
	}
	raw, err := commercepolicy.Encode(policy.Policy)
	if err != nil {
		return false, fmt.Errorf("orders policy replica: %w", err)
	}
	var stored bool
	err = r.runInTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		n, qerr := r.q.WithTx(tx).StorePolicyReplicaIfNewer(ctx, db.StorePolicyReplicaIfNewerParams{
			TenantID: pgconv.PgUUID(tid), Version: policy.Version, Policy: raw,
		})
		stored = n > 0
		return qerr
	})
	if err != nil {
		return false, fmt.Errorf("orders policy replica: store: %w", err)
	}
	return stored, nil
}

// ReadPolicy satisfies [query.PolicyReader]: the stored policy, or the defaults
// at version 0 (commercepolicy.Initial) when none is stored.
func (r *PolicyReplicaRepository) ReadPolicy(ctx context.Context, tenantID tenant.ID) (commercepolicy.Versioned, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return commercepolicy.Versioned{}, fmt.Errorf("orders policy replica: parse tenant id: %w", err)
	}
	var out commercepolicy.Versioned
	err = r.runInTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, gerr := loadPolicyReplica(ctx, r.q.WithTx(tx), tid)
		out = got
		return gerr
	})
	if errors.Is(err, ErrPolicyReplicaNotFound) {
		return commercepolicy.Initial(), nil
	}
	if err != nil {
		return commercepolicy.Versioned{}, fmt.Errorf("orders policy replica: read: %w", err)
	}
	return out, nil
}

// loadPolicyReplica reads one row, mapping a missing one to
// [ErrPolicyReplicaNotFound].
func loadPolicyReplica(ctx context.Context, q *db.Queries, tid uuid.UUID) (commercepolicy.Versioned, error) {
	row, err := q.GetPolicyReplica(ctx, pgconv.PgUUID(tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return commercepolicy.Versioned{}, ErrPolicyReplicaNotFound
	}
	if err != nil {
		return commercepolicy.Versioned{}, err
	}
	pol, err := commercepolicy.Decode(row.Policy)
	if err != nil {
		return commercepolicy.Versioned{}, err
	}
	return commercepolicy.Versioned{Version: row.Version, Policy: pol}, nil
}
