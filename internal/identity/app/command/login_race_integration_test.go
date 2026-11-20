//go:build integration

// arch-test:no-timeout-needed — shares the package's pgtest container; execution
//   is bounded by the pgxpool conn timeouts and the package-level test timeout.
//
// arch-test:parallel-safe — every test registers its own tenant and person with
//   a unique email, and the phantom counter is keyed per address.
//
// arch-test:no-synctest — real Postgres wall-clock waits (lock and barrier proofs).
//
// arch-test:raw-sql-justified — the lock proof reads pg_stat_activity (is the
//   share-lock read waiting?) straight from the server.

package command_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

const raceAdminPassword = "Correct-Horse-Battery-9!"

// changePasswordFor builds the real change-password handler over the wired pool.
func changePasswordFor(app wiredApp) command.ChangePasswordHandler {
	tx := pg.NewTransactor(app.pool)
	return command.NewChangePasswordHandler(
		adapters.NewPersonRepository(app.pool, tx),
		adapters.NewRefreshTokenFamilyRepository(app.pool, tx),
		adapters.NewTenantRepository(app.pool, tx),
		tx, passwordpolicy.Noop{}, func() time.Time { return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC) },
	)
}

func personOf(t *testing.T, app wiredApp, addr email.Address) *person.Person {
	t.Helper()
	p, err := adapters.NewPersonRepository(app.pool, pg.NewTransactor(app.pool)).GetByEmail(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func activeFamilies(t *testing.T, app wiredApp, addr email.Address) int {
	t.Helper()
	p := personOf(t, app, addr)
	fams, err := adapters.NewRefreshTokenFamilyRepository(app.pool, pg.NewTransactor(app.pool)).ListActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatal(err)
	}
	return len(fams)
}

// Item 1, deterministic, against Postgres: the password change commits between the
// login's verify and its family insert. The login answers 401 and no family row exists.
func TestLogin_PasswordChangeCommitsBetweenVerifyAndInsert_NoSessionExists(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	addr := registerRealAccount(t, app)
	p := personOf(t, app, addr)
	oldHash := p.PasswordHash().String()
	change := changePasswordFor(app)

	var raced atomic.Bool
	h := app.login.WithPasswordVerifier(func(password, encoded string) error {
		verr := argon2.Verify(password, encoded)
		if verr == nil && encoded == oldHash && raced.CompareAndSwap(false, true) {
			if err := change.Handle(t.Context(), command.ChangePasswordCommand{
				PersonID: p.ID(), CurrentPassword: raceAdminPassword, NewPassword: "Brand-New-Passphrase-77!",
			}); err != nil {
				t.Errorf("password change in the gap: %v", err)
			}
		}
		return verr
	})
	_, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: raceAdminPassword, DeviceLabel: "attacker"})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("a login that verified the old password after the change = %v, want ErrInvalidCredentials", err)
	}
	if n := activeFamilies(t, app, addr); n != 0 {
		t.Fatalf("%d active families for a refused login, want 0", n)
	}
}

// The share lock itself: a password writer holds the row; the login's session-insert
// read waits for it and then sees the NEW hash. Proven with the database's own view of
// who waits (pg_stat_activity), not with a sleep.
func TestPersonShareLock_WaitsForAPasswordWriterAndReadsItsResult(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	addr := registerRealAccount(t, app)
	tx := pg.NewTransactor(app.pool)
	persons := adapters.NewPersonRepository(app.pool, tx)
	p := personOf(t, app, addr)
	newHashStr, err := argon2.Hash("another-strong-passphrase-1!")
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := person.NewPasswordHash(newHashStr)
	if err != nil {
		t.Fatal(err)
	}

	holding, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- persons.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) {
			close(holding) // the row is locked FOR NO KEY UPDATE from here
			<-release
			return true, loaded.ChangePassword(newHash, time.Now())
		})
	}()
	<-holding

	type read struct {
		hash string
		err  error
	}
	got := make(chan read, 1)
	go func() {
		var r read
		r.err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
			cur, err := persons.GetByIDForShare(ctx, p.ID())
			if err == nil {
				r.hash = cur.PasswordHash().String()
			}
			return err
		})
		got <- r
	}()

	// Wait until the database reports the share-lock read as waiting on a lock.
	deadline := time.Now().Add(10 * time.Second)
	for { // arch-test:wait-justified polling the server's own wait state, bounded by the deadline
		var waiting int
		if err := app.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%FOR SHARE%' AND query LIKE '%identity.persons%'`,
		).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the share-lock read never waited for the password writer")
		}
		time.Sleep(10 * time.Millisecond) // arch-test:wait-justified polling the server's own wait state, bounded by the deadline above
	}
	select {
	case r := <-got:
		t.Fatalf("the share-lock read returned (%v) while the writer held the row", r.err)
	default:
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.hash != newHash.String() {
		t.Fatal("the share-lock read did not see the committed password")
	}
}

// Item 1, race: logins with the OLD password loop while the password changes. When
// everything has settled no active family exists and no token any of them returned
// refreshes. Run -count=20.
func TestLogin_RaceWithPasswordChange_LeavesNoSurvivingSession(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	addr := registerRealAccount(t, app)
	p := personOf(t, app, addr)
	change := changePasswordFor(app)

	const workers = 6
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		tokens    []string
		okLogins  atomic.Int32
		changed   atomic.Bool
		startedUp = make(chan struct{})
		once      sync.Once
	)
	for range workers {
		wg.Go(func() {
			for !changed.Load() {
				res, err := app.login.Handle(t.Context(), command.LoginCommand{Email: addr, Password: raceAdminPassword, DeviceLabel: "old-password"})
				switch {
				case err == nil:
					mu.Lock()
					tokens = append(tokens, res.RefreshTokenPlain)
					mu.Unlock()
					if okLogins.Add(1) >= 2 {
						once.Do(func() { close(startedUp) })
					}
				case errors.Is(err, command.ErrInvalidCredentials), errors.Is(err, command.ErrAccountLocked):
				default:
					t.Errorf("login: %v", err)
					return
				}
			}
		})
	}
	select {
	case <-startedUp:
	case <-time.After(30 * time.Second):
		changed.Store(true)
		wg.Wait()
		t.Fatal("no old-password login succeeded before the change")
	}
	err := change.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID: p.ID(), CurrentPassword: raceAdminPassword, NewPassword: "Brand-New-Passphrase-77!",
	})
	changed.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatalf("change password: %v", err)
	}

	if n := activeFamilies(t, app, addr); n != 0 {
		t.Fatalf("%d active families survive the password change, want 0", n)
	}
	for i, tok := range tokens {
		if _, err := app.refresh.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: tok}); err == nil {
			t.Fatalf("token %d of %d from an old-password login still refreshes after the change", i, len(tokens))
		}
	}
}

// Item 2, the batch: 39 wrong attempts and then one correct one, behind a barrier that
// holds every verify until all 40 are inside one. At most threshold verifies run against
// the real hash (counted through the injected verifier, not by counting answers), the
// correct password does not sign in, and the lock stands afterwards.
func TestLogin_ABatchWithOneCorrectPassword_CannotLogInAndRunsAtMostThresholdRealVerifies(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	addr := registerRealAccount(t, app)
	realHash := personOf(t, app, addr).PasswordHash().String()

	const wrong = 39
	var (
		arrived, realVerifies atomic.Int32
		open                  = make(chan struct{})
		openOnce              sync.Once
	)
	h := app.login.WithPasswordVerifier(func(password, encoded string) error {
		if encoded == realHash {
			realVerifies.Add(1)
		}
		if arrived.Add(1) == wrong+1 {
			openOnce.Do(func() { close(open) })
		}
		<-open
		return argon2.Verify(password, encoded)
	})

	var (
		wg      sync.WaitGroup
		results [wrong + 1]error
	)
	for i := range wrong {
		wg.Go(func() {
			_, results[i] = h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: "WRONG-batch", DeviceLabel: "t"})
		})
	}
	// The correct password arrives after the others are inside their verify (and so
	// after their registrations): the account is already past the threshold for it.
	for arrived.Load() < wrong {
		time.Sleep(5 * time.Millisecond) // arch-test:wait-justified polling an atomic counter, the barrier opens the moment the last attempt arrives
	}
	_, results[wrong] = h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: raceAdminPassword, DeviceLabel: "t"})
	wg.Wait()

	if results[wrong] == nil {
		t.Fatal("the correct password signed in while the account was locked by the batch")
	}
	if !errors.Is(results[wrong], command.ErrAccountLocked) {
		t.Fatalf("the correct password after the batch = %v, want ErrAccountLocked", results[wrong])
	}
	if n := realVerifies.Load(); n > person.MaxFailedLogins {
		t.Fatalf("%d verifies ran against the real hash, want at most %d", n, person.MaxFailedLogins)
	}
	if n := activeFamilies(t, app, addr); n != 0 {
		t.Fatalf("%d sessions opened by the batch", n)
	}
	if _, err := app.login.Handle(t.Context(), command.LoginCommand{Email: addr, Password: raceAdminPassword, DeviceLabel: "t"}); !errors.Is(err, command.ErrAccountLocked) {
		t.Fatalf("after the batch: %v, want ErrAccountLocked", err)
	}
}

// A correct password whose verify straddles the failures that lock the account never
// clears the lock: the failures land between its registration and its success reset.
func TestLogin_CorrectPasswordStraddlingLockingFailures_NeverClearsTheLock(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	addr := registerRealAccount(t, app)
	realHash := personOf(t, app, addr).PasswordHash().String()

	var raced atomic.Bool
	h := app.login.WithPasswordVerifier(func(password, encoded string) error {
		verr := argon2.Verify(password, encoded)
		if verr == nil && encoded == realHash && raced.CompareAndSwap(false, true) {
			// Inside the correct attempt's verify the rest of a batch locks the account.
			for range person.MaxFailedLogins {
				_, _ = app.login.Handle(t.Context(), command.LoginCommand{Email: addr, Password: "WRONG-straddle", DeviceLabel: "t"})
			}
		}
		return verr
	})
	_, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: raceAdminPassword, DeviceLabel: "t"})
	if !errors.Is(err, command.ErrAccountLocked) {
		t.Fatalf("correct password straddling the locking failures = %v, want ErrAccountLocked", err)
	}
	if n := activeFamilies(t, app, addr); n != 0 {
		t.Fatalf("%d sessions for a login answered locked", n)
	}
	if _, err := app.login.Handle(t.Context(), command.LoginCommand{Email: addr, Password: raceAdminPassword, DeviceLabel: "t"}); !errors.Is(err, command.ErrAccountLocked) {
		t.Fatalf("the lock was cleared: %v", err)
	}
}
