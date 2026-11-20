//go:build integration

package command_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
)

// The one place the integration rig builds the two password handlers.
func newChangePasswordHandler(pool *pgxpool.Pool, persons *adapters.PersonRepository, families *adapters.RefreshTokenFamilyRepository, tx *pg.Transactor, now func() time.Time) command.ChangePasswordHandler {
	return command.NewChangePasswordHandler(persons, families, adapters.NewTenantRepository(pool, tx), tx, passwordpolicy.Noop{}, now)
}

func newConfirmPasswordResetHandler(pool *pgxpool.Pool, persons *adapters.PersonRepository, families *adapters.RefreshTokenFamilyRepository, tx *pg.Transactor, now func() time.Time) command.ConfirmPasswordResetHandler {
	return command.NewConfirmPasswordResetHandler(persons, families, adapters.NewMembershipRepository(pool, tx), adapters.NewTenantRepository(pool, tx), tx, passwordpolicy.Noop{}, now)
}
