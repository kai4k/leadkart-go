//go:build integration

// arch-test:no-timeout-needed — every call carries t.Context().

package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

// JoinTxPgxTenant through the real unit-of-work entry points on a real pool: the scope and tenant
// each entry point records are what the join checks (the unit test builds the ctx by hand and so
// cannot see a missing recording). Review G2i S1.
func TestJoinTxPgxTenant_ThroughTheRealUnitOfWork(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), startPostgres(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	tr := pg.NewTransactor(pool)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	noop := func(context.Context, pgx.Tx) error { return nil }
	join := func(ctx context.Context, tenant string) error { return tr.JoinTxPgxTenant(ctx, tenant, noop) }

	tests := []struct {
		name string
		run  func(t *testing.T) error
		want error
	}{
		{"WithinTenantTx joins its own tenant", func(t *testing.T) error {
			return tr.WithinTenantTx(t.Context(), tenantA, func(ctx context.Context) error { return join(ctx, tenantA) })
		}, nil},
		{"WithinTenantTx refuses another tenant", func(t *testing.T) error {
			return tr.WithinTenantTx(t.Context(), tenantA, func(ctx context.Context) error { return join(ctx, tenantB) })
		}, pg.ErrJoinTenantMismatch},
		{"WithinTx(tenant) joins its own tenant", func(t *testing.T) error {
			ctx := tenancy.WithID(t.Context(), tenancy.ID(tenantA))
			return tr.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error { return join(ctx, tenantA) })
		}, nil},
		{"WithinTx(tenant) refuses another tenant", func(t *testing.T) error {
			ctx := tenancy.WithID(t.Context(), tenancy.ID(tenantA))
			return tr.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error { return join(ctx, tenantB) })
		}, pg.ErrJoinTenantMismatch},
		{"WithinTx(platform) is refused", func(t *testing.T) error {
			return tr.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error { return join(ctx, tenantA) })
		}, pg.ErrJoinUnscopedTx},
		{"platform then tenant: the inner tenant tx is joined", func(t *testing.T) error {
			return tr.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
				return tr.WithinTenantTx(ctx, tenantA, func(ctx context.Context) error { return join(ctx, tenantA) })
			})
		}, nil},
		{"tenant then platform: the inner platform tx is refused", func(t *testing.T) error {
			return tr.WithinTenantTx(t.Context(), tenantA, func(ctx context.Context) error {
				return tr.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error { return join(ctx, tenantA) })
			})
		}, pg.ErrJoinUnscopedTx},
	}
	for _, tc := range tests {
		// arch-test:serial — the subtests share one pool and one container.
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(t); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
