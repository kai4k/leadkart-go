package pg

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type fakeTx struct{ pgx.Tx }

func TestJoinTxPgxTenant_ScopeChecks(t *testing.T) {
	t.Parallel()

	base := contextWithTx(t.Context(), fakeTx{})
	tests := []struct {
		name    string
		ctx     context.Context
		wantErr error
	}{
		{"match", contextWithTxScope(base, txScope{scope: TxScopeTenant, tenant: "t1"}), nil},
		{"mismatch", contextWithTxScope(base, txScope{scope: TxScopeTenant, tenant: "t2"}), ErrJoinTenantMismatch},
		{"platform", contextWithTxScope(base, txScope{scope: TxScopePlatform}), ErrJoinUnscopedTx},
		{"unscoped", base, ErrJoinUnscopedTx},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &Transactor{} // nil pool: a join must never begin a tx
			called := false
			err := tr.JoinTxPgxTenant(tc.ctx, "t1", func(context.Context, pgx.Tx) error {
				called = true
				return nil
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if called != (tc.wantErr == nil) {
				t.Fatalf("fn called = %v, want %v", called, tc.wantErr == nil)
			}
		})
	}
}
