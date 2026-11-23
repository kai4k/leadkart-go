//go:build integration

package adapters

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ReadAsPlatformOnTxForTest exposes the savepoint mechanism behind
// ListActiveRefsForPersonInTx so a test can make the read inside it fail.
func ReadAsPlatformOnTxForTest(ctx context.Context, tx pgx.Tx, read func(sp pgx.Tx) error) error {
	return readAsPlatformOnTx(ctx, tx, read)
}
