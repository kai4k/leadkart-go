package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

// SetTenantOnTx binds the current request's tenant to a Postgres
// transaction so RLS policies on tenant-scoped tables fire correctly.
//
// Reads [tenancy.FromContext], runs `SELECT set_config('app.tenant_id',
// $1, true)` (transaction-local), and returns nil. If the context has no
// tenant, returns an error rather than silently emitting unscoped queries
// (fail-closed per multi-tenancy.md).
//
// Callers MUST invoke this as the first statement of every read/write
// transaction that touches tenant-scoped tables. Cross-tenant operations
// must instead call [SetPlatformOnTx].
func SetTenantOnTx(ctx context.Context, tx pgx.Tx) error {
	id, ok := tenancy.FromContext(ctx)
	if !ok {
		return fmt.Errorf("pg: tenant context required (use SetPlatformOnTx for cross-tenant flows)")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, id.String()); err != nil {
		return fmt.Errorf("pg: bind tenant_id GUC: %w", err)
	}
	return nil
}

// SetPlatformOnTx flips the platform-operator GUC for the duration of the
// transaction, bypassing RLS on every tenant-scoped table. Reserved for:
//
//   - The outbox forwarder (drains events across tenants).
//   - Cross-tenant maintenance (anonymise / global-suspend / impersonation
//     audit writes / system metrics).
//   - The bootstrap migration (no tenant context exists yet).
//
// Per ADR 0006: every other code path uses [SetTenantOnTx].
func SetPlatformOnTx(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.is_platform', 'true', true)`); err != nil {
		return fmt.Errorf("pg: bind is_platform GUC: %w", err)
	}
	return nil
}
