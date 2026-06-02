package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

// GUC names are the single source of truth for the Postgres session
// settings that RLS policies consult via current_setting(...). Every
// set_config call (production + test helpers in rlstest) binds these
// constants as the setting-name parameter so a rename can't desync the
// writer from the policy. The names MUST match the current_setting()
// references in migrations/*.sql (the RLS policy definitions).
const (
	// GUCTenantID is the per-transaction tenant scope RLS reads to
	// filter tenant-owned rows.
	GUCTenantID = "app.tenant_id"
	// GUCIsPlatform is the platform-operator escape hatch that bypasses
	// tenant RLS for cross-tenant flows.
	GUCIsPlatform = "app.is_platform"
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
		return errors.New("pg: tenant context required (use SetPlatformOnTx for cross-tenant flows)")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, GUCTenantID, id.String()); err != nil {
		return fmt.Errorf("pg: bind tenant_id GUC: %w", err)
	}
	return nil
}

// SetTenantOnTxExplicit binds the supplied tenant.ID to the Postgres
// transaction's app.tenant_id GUC. The TDL-canon variant of
// [SetTenantOnTx] — the tenantID is an EXPLICIT parameter rather than
// fished out of context.Context. Per ADR 0062 + the "domain values
// belong in function signatures, not in ctx" principle (Khorikov §11 +
// Cheney accept-interfaces-return-structs).
//
// Adapters that have refactored to explicit-tenant Repository methods
// call this; ctx-tenancy.WithID can be empty. ctx is still passed for
// cancellation/deadline.
//
// Empty tenantID returns an error (use [SetPlatformOnTx] for cross-
// tenant flows; fail-closed per multi-tenancy.md).
func SetTenantOnTxExplicit(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if tenantID == "" {
		return errors.New("pg: explicit tenantID required (use SetPlatformOnTx for cross-tenant flows)")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, GUCTenantID, tenantID); err != nil {
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
	if _, err := tx.Exec(ctx, `SELECT set_config($1, 'true', true)`, GUCIsPlatform); err != nil {
		return fmt.Errorf("pg: bind is_platform GUC: %w", err)
	}
	return nil
}
