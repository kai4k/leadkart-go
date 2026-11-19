package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// AuthorityLockPG implements command.AuthorityLocker (ADR 0069) with
// pg_advisory_xact_lock: released automatically at commit or rollback. It MUST
// run inside a pg.UnitOfWork transaction; a lock taken outside one would be
// released immediately and protect nothing, so it is refused.
type AuthorityLockPG struct{}

// NewAuthorityLockPG wires the lock (stateless).
func NewAuthorityLockPG() *AuthorityLockPG { return &AuthorityLockPG{} }

// LockTenantAuthority blocks until the tenant's authority lock is held by the ambient tx.
func (*AuthorityLockPG) LockTenantAuthority(ctx context.Context, tenantID tenant.ID) error {
	tx, ok := pg.TxFromContext(ctx)
	if !ok {
		return errors.New("authority lock: requires an ambient UnitOfWork transaction")
	}
	u, err := uuid.Parse(tenantID.String())
	if err != nil {
		return fmt.Errorf("authority lock: parse tenant id %q: %w", tenantID, err)
	}
	if err := db.New(tx).LockTenantAuthority(ctx, pgconv.PgUUID(u)); err != nil {
		return fmt.Errorf("authority lock: %w", err)
	}
	return nil
}
