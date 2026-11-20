package command

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// revokeAllFamilies ends every active refresh family of the person. It runs in
// the caller's unit of work (ctx carries the transaction), AFTER the person row
// was locked and rewritten by persons.UpdateByID: the lock order is person row,
// then family rows, and nothing that holds a family lock takes a person lock, so
// two writers cannot wait on each other. Each family is reloaded under its own
// row lock inside UpdateByID, so a rotation that was in flight finishes first and
// is then revoked; a revoked family stays revoked.
//
// Used by the two credential-replacing commands (change-password, confirm
// password reset). The access token carries no family id, so the caller's own
// family cannot be told from the others and every family is ended.
func revokeAllFamilies(ctx context.Context, families refreshtoken.Repository, personID person.ID, reason string, now time.Time) error {
	active, err := families.ListActiveForPerson(ctx, personID)
	if err != nil {
		return fmt.Errorf("list active families: %w", err)
	}
	for _, f := range active {
		if err := families.UpdateByID(ctx, f.ID(), func(loaded *refreshtoken.Family) (bool, error) {
			if loaded.IsRevoked() {
				return false, nil
			}
			if err := loaded.Revoke(reason, now); err != nil {
				return false, err
			}
			return true, nil
		}); err != nil {
			return fmt.Errorf("revoke family %s: %w", f.ID(), err)
		}
	}
	return nil
}
