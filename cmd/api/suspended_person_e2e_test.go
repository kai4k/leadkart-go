//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context().
//
// arch-test:raw-sql-justified — counting the refresh families of a person and
//   reading the stored reset-attempt counter have no HTTP surface.

// Fix round 13, item 1: a globally suspended person is "no account" on every path
// that would give them a session or a credential, and lifting the suspension
// restores them. The cascade (families revoked asynchronously) is deliberately NOT
// started here: these tests prove the request paths refuse on their own.
package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) suspend(t *testing.T, personID string) {
	t.Helper()
	if err := f.app.Commands.GlobalSuspendPerson.Handle(t.Context(), command.GlobalSuspendPersonCommand{
		PersonID: person.ID(personID), Reason: "e2e suspension",
	}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	f.evictStamp(t, personID)
}

func (f e2eFixture) lift(t *testing.T, personID string) {
	t.Helper()
	if err := f.app.Commands.LiftPersonGlobalSuspension.Handle(t.Context(), command.LiftPersonGlobalSuspensionCommand{PersonID: person.ID(personID)}); err != nil {
		t.Fatalf("lift: %v", err)
	}
}

func (f e2eFixture) familyCount(t *testing.T, personID string) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM identity.refresh_token_families WHERE person_id = $1`, personID).Scan(&n); err != nil {
		t.Fatalf("count families: %v", err)
	}
	return n
}

func TestE2E_SuspendedPerson_CannotLogInRefreshOrReset_AndLiftRestores(t *testing.T) {
	f := newE2EFixture(t) // no stamp/revocation cascade: the paths must refuse by themselves; the clock is pinned, so the seeded reset token is live
	owner := f.registerAndLogin(t, "suspme")
	m := f.createMember(t, owner, "banned")
	pair := f.loginPair(t, m.email, m.password) // a session that exists BEFORE the suspension
	resetToken := f.seedResetToken(t, m.personID)
	before := f.familyCount(t, m.personID)

	f.suspend(t, m.personID)

	// Login with the CORRECT password: 401, no new family.
	r := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "banned"})
	if r.status != http.StatusUnauthorized || decodeError(t, r.body).Error != ports.ErrCodeInvalidCredentials {
		t.Fatalf("suspended login: %d %s, want 401 invalid_credentials", r.status, r.body)
	}
	if got := f.familyCount(t, m.personID); got != before {
		t.Fatalf("families %d -> %d: a suspended login must not open one", before, got)
	}

	// Refresh with the token of the session that existed before: 401.
	if r := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken}); r.status != http.StatusUnauthorized {
		t.Fatalf("suspended refresh: %d %s, want 401", r.status, r.body)
	}

	// Reset request: the same 204 as an unknown address.
	susp := f.postJSON(t, "/api/v1/auth/request-password-reset", ports.RequestPasswordResetRequest{Email: m.email})
	unknown := f.postJSON(t, "/api/v1/auth/request-password-reset", ports.RequestPasswordResetRequest{Email: "nobody-" + ids.NewV7().String()[24:] + "@e2e.test"})
	if susp.status != http.StatusNoContent || surfaceOf(susp) != surfaceOf(unknown) {
		t.Fatalf("suspended reset request %+v, unknown %+v", surfaceOf(susp), surfaceOf(unknown))
	}

	// Reset confirm with a token minted before the suspension: refused, and the
	// password is unchanged (proved below by signing in with it after the lift).
	r = f.postJSON(t, "/api/v1/auth/reset-password", ports.ResetPasswordRequest{Token: resetToken, NewPassword: "Reset-Pass-2026!suspended"})
	if r.status != http.StatusBadRequest || decodeError(t, r.body).Error != ports.ErrCodeResetTokenInvalid {
		t.Fatalf("suspended reset confirm: %d %s, want 400 reset_token_invalid", r.status, r.body)
	}

	// Lifting restores sign-in with the ORIGINAL password.
	f.lift(t, m.personID)
	if r := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "back"}); r.status != http.StatusOK {
		t.Fatalf("login after the lift: %d %s, want 200", r.status, r.body)
	}
}
