//go:build integration

// arch-test:no-timeout-needed — shared pgtest container; the package-level
//   `task ci:test:int -timeout=15m` bounds execution.
//
// arch-test:parallel-safe — every test registers its own tenant + person with a
//   unique email; no cross-test rows are read.
//
// arch-test:no-synctest — real Postgres transactions and real row locks; the
//   race test exercises the database's concurrency, which a virtual clock cannot model.

// Task 7/8 fix round, item 1: a password change ends the person's refresh
// families IN THE SAME TRANSACTION as the password write. No worker runs in this
// package, so everything asserted here holds without the outbox subscriber.

package command_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

const (
	rigOldPassword = "Correct-Horse-Battery-9!"
	rigNewPassword = "Tr0ub4dor&3-newly-strong-passphrase!"
)

// sessionRig is one registered person plus the handlers that end their sessions.
type sessionRig struct {
	app      wiredApp
	persons  *adapters.PersonRepository
	families *adapters.RefreshTokenFamilyRepository
	change   command.ChangePasswordHandler
	confirm  command.ConfirmPasswordResetHandler
	email    email.Address
	personID person.ID
}

func newSessionRig(t *testing.T) sessionRig {
	t.Helper()
	app := newWiredApp(t)
	tx := pg.NewTransactor(app.pool)
	persons := adapters.NewPersonRepository(app.pool, tx)
	families := adapters.NewRefreshTokenFamilyRepository(app.pool, tx)
	now := func() time.Time { return time.Now().UTC() }

	suffix := ids.NewV7().String()
	sl, err := slug.New("pws-" + suffix[len(suffix)-10:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, err := email.New("pws-" + suffix[len(suffix)-10:] + "@sessions.test")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	reg, err := app.register.Handle(t.Context(), command.RegisterTenantCommand{
		Slug: sl, LegalName: "Sessions Pharma Pvt Ltd", DisplayName: "Sessions",
		AdminEmail: addr, AdminPassword: rigOldPassword, AdminFirstName: "Sam", AdminLastName: "Sessions",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return sessionRig{
		app: app, persons: persons, families: families,
		change:   newChangePasswordHandler(app.pool, persons, families, tx, now),
		confirm:  newConfirmPasswordResetHandler(app.pool, persons, families, tx, now),
		email:    addr,
		personID: reg.PersonID,
	}
}

// login opens a new session (a new refresh family) for the rig's person.
func (r sessionRig) login(t *testing.T, password string) command.LoginResult {
	t.Helper()
	out, err := r.app.login.Handle(t.Context(), command.LoginCommand{Email: r.email, Password: password, DeviceLabel: "rig"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return out
}

func (r sessionRig) familyOf(t *testing.T, refreshPlain string) *refreshtoken.Family {
	t.Helper()
	h, err := refreshtoken.NewTokenHash(refreshmint.HashOf(refreshPlain))
	if err != nil {
		t.Fatalf("token hash: %v", err)
	}
	f, err := r.families.GetByTokenHash(t.Context(), h)
	if err != nil {
		t.Fatalf("family by token: %v", err)
	}
	return f
}

func (r sessionRig) changePassword(t *testing.T) {
	t.Helper()
	if err := r.change.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID: r.personID, CurrentPassword: rigOldPassword, NewPassword: rigNewPassword,
	}); err != nil {
		t.Errorf("change password: %v", err)
	}
}

func (r sessionRig) stamp(t *testing.T) string {
	t.Helper()
	p, err := r.persons.GetByID(t.Context(), r.personID)
	if err != nil {
		t.Fatalf("load person: %v", err)
	}
	return p.SecurityStamp().String()
}

// Scenario A of the review: someone who knew the temporary password logged in
// first and kept the refresh token; the real user then changes the password.
// With no worker running, every other session must already be dead.
func TestPasswordChange_EndsEveryRefreshFamilyWithoutAWorker(t *testing.T) {
	t.Parallel()
	r := newSessionRig(t)
	intruder := r.login(t, rigOldPassword)
	owner := r.login(t, rigOldPassword)

	r.changePassword(t)

	for name, plain := range map[string]string{"the other session": intruder.RefreshTokenPlain, "the caller's own session": owner.RefreshTokenPlain} {
		if _, err := r.app.refresh.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: plain}); !errors.Is(err, command.ErrRefreshRejected) {
			t.Errorf("%s refreshed after the password change: err = %v; want ErrRefreshRejected", name, err)
		}
		if f := r.familyOf(t, plain); !f.IsRevoked() || f.RevokeReason() != "password_changed" {
			t.Errorf("%s: family revoked=%v reason=%q; want revoked with reason password_changed", name, f.IsRevoked(), f.RevokeReason())
		}
	}
	active, err := r.families.ListActiveForPerson(t.Context(), r.personID)
	if err != nil || len(active) != 0 {
		t.Fatalf("active families after the change = %d (err %v); want 0", len(active), err)
	}

	// The new password opens a new session that refreshes to an unflagged token.
	fresh := r.login(t, rigNewPassword)
	if fresh.MustChangePassword {
		t.Fatal("login after the change must not carry the flag")
	}
	got, err := r.app.refresh.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fresh.RefreshTokenPlain})
	if err != nil || got.MustChangePassword {
		t.Fatalf("refresh of the post-change session: err %v flag %v; want ok, unflagged", err, got.MustChangePassword)
	}
}

// A refused refresh must not look like reuse: the family was revoked by the
// change, the answer is the single ErrRefreshRejected, and the revocation reason
// stays password_changed.
func TestPasswordChange_RefreshOfARevokedFamilyKeepsTheRevokeReason(t *testing.T) {
	t.Parallel()
	r := newSessionRig(t)
	s := r.login(t, rigOldPassword)
	r.changePassword(t)
	for range 2 {
		if _, err := r.app.refresh.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: s.RefreshTokenPlain}); !errors.Is(err, command.ErrRefreshRejected) {
			t.Fatalf("refresh = %v; want ErrRefreshRejected", err)
		}
	}
	if f := r.familyOf(t, s.RefreshTokenPlain); f.RevokeReason() != "password_changed" {
		t.Fatalf("revoke reason = %q; want password_changed", f.RevokeReason())
	}
}

// The anonymous reset path ends every session too, in the same transaction.
func TestPasswordResetConfirm_EndsEveryRefreshFamilyWithoutAWorker(t *testing.T) {
	t.Parallel()
	r := newSessionRig(t)
	a := r.login(t, rigOldPassword)
	b := r.login(t, rigOldPassword)

	const rawToken = "reset-token-plaintext-0123456789abcdef"
	sum := sha256.Sum256([]byte(rawToken))
	hash, err := person.NewPasswordResetTokenHash(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.persons.UpdateByID(t.Context(), r.personID, func(p *person.Person) (bool, error) {
		return true, p.RequestPasswordReset(rawToken, hash, time.Hour, time.Now().UTC())
	}); err != nil {
		t.Fatalf("request reset: %v", err)
	}
	if err := r.confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: rawToken, NewPassword: rigNewPassword}); err != nil {
		t.Fatalf("confirm reset: %v", err)
	}

	for name, plain := range map[string]string{"first": a.RefreshTokenPlain, "second": b.RefreshTokenPlain} {
		if _, err := r.app.refresh.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: plain}); !errors.Is(err, command.ErrRefreshRejected) {
			t.Errorf("%s session refreshed after the reset: err = %v; want ErrRefreshRejected", name, err)
		}
	}
}

// A rotation that commits just before the change, whose person read lands just
// after it, must not hand out an unflagged token from a revoked family. Whatever
// the interleaving: the family ends revoked, and a refresh that succeeded carries
// the OLD stamp (dead on arrival), never the new one. Run with -count=20.
//
// This test is an interleaving SWEEP, not a proof: the scheduler picks the
// interleaving, so a single run may never hit the narrow one. The proof is the
// deterministic sibling TestFamilyUpdate_RotationAndRevocationSerialiseOnTheFamilyRow
// (the two writers are ordered by a barrier on the family row lock) together with
// TestRefresh_FamilyRevokedAfterThePersonRead_IsRejected (the revoke lands exactly
// between refresh's person read and its family re-read); this one guards against an
// interleaving neither of them names.
func TestPasswordChange_ConcurrentRefreshNeverYieldsAUsableTokenFromARevokedFamily(t *testing.T) {
	t.Parallel()
	r := newSessionRig(t)
	s := r.login(t, rigOldPassword)
	oldStamp := r.stamp(t)

	var wg sync.WaitGroup
	var refreshed command.RefreshResult
	var refreshErr error
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		refreshed, refreshErr = r.app.refresh.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: s.RefreshTokenPlain})
	}()
	go func() {
		defer wg.Done()
		<-start
		r.changePassword(t)
	}()
	close(start)
	wg.Wait()

	if f := r.familyOf(t, s.RefreshTokenPlain); !f.IsRevoked() {
		t.Fatal("the family survived the password change")
	}
	if refreshErr == nil {
		claims, err := r.app.issuer.Verify(refreshed.AccessToken)
		if err != nil {
			t.Fatalf("verify refreshed token: %v", err)
		}
		if claims.SecurityStamp != oldStamp {
			t.Fatalf("a refresh racing the change minted a token with the NEW stamp (%s != %s): usable and unflagged", claims.SecurityStamp, oldStamp)
		}
	} else if !errors.Is(refreshErr, command.ErrRefreshRejected) {
		t.Fatalf("refresh error = %v; want nil or ErrRefreshRejected", refreshErr)
	}
}

// The family row lock, deterministically: a rotation that has loaded the family
// and is still open blocks a revocation of the same family until it commits, and
// the revocation then lands on top of the rotation instead of being overwritten by
// it. Without the lock the revocation finishes at once and the rotation's write
// puts revoked_at back to NULL.
func TestFamilyUpdate_RotationAndRevocationSerialiseOnTheFamilyRow(t *testing.T) {
	t.Parallel()
	r := newSessionRig(t)
	s := r.login(t, rigOldPassword)
	fam := r.familyOf(t, s.RefreshTokenPlain)
	presented, err := refreshtoken.NewTokenHash(refreshmint.HashOf(s.RefreshTokenPlain))
	if err != nil {
		t.Fatal(err)
	}
	next, err := refreshmint.Mint()
	if err != nil {
		t.Fatal(err)
	}

	loaded := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	// A failed assertion must not leave the rotation parked on the channel: the pool
	// would wait for its transaction forever and the run would hang instead of fail.
	t.Cleanup(free)
	rotated := make(chan error, 1)
	go func() {
		rotated <- r.families.UpdateByID(t.Context(), fam.ID(), func(f *refreshtoken.Family) (bool, error) {
			close(loaded)
			<-release
			return true, f.Rotate(presented, next.Hash, time.Hour, fam.CreatedAt().Add(time.Minute))
		})
	}()
	<-loaded

	revoked := make(chan error, 1)
	go func() {
		revoked <- r.families.UpdateByID(t.Context(), fam.ID(), func(f *refreshtoken.Family) (bool, error) {
			return true, f.Revoke("password_changed", time.Now().UTC())
		})
	}()
	select {
	case err := <-revoked:
		t.Fatalf("the revocation finished while a rotation held the family open (err %v): no row lock", err)
	case <-time.After(500 * time.Millisecond):
	}
	free()
	if err := <-rotated; err != nil {
		t.Fatalf("rotation: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("revocation: %v", err)
	}
	if f := r.familyOf(t, next.Plaintext); !f.IsRevoked() || f.RevokeReason() != "password_changed" {
		t.Fatalf("after both commits revoked=%v reason=%q; want the revocation to survive the rotation", f.IsRevoked(), f.RevokeReason())
	}
}
