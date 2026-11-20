//go:build integration

// arch-test:no-timeout-needed — shares the package's pgtest container; execution
//   is bounded by the pgxpool conn timeouts and the package-level test timeout.
//
// arch-test:raw-sql-justified — TestPhantomLockoutRepository_StoresNoEmailOnlyAnHMAC
//   and the purge test read identity.login_phantom_failures directly: what is IN
//   the table (and that it holds no address) is the contract, and the adapter
//   deliberately exposes no read of keys.
//
// SQL-CONTRACT COVERAGE (ADR 0062): the atomic failed-login statements of
// identity.persons (RegisterPersonFailedLogin) and identity.login_phantom_failures
// (RegisterPhantomLoginFailure) behave exactly like the aggregate's own policy
// (person.RegisterFailedLogin), serialise concurrent attempts on the row, and the
// phantom table holds no address.

package adapters_test

import (
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

var lockoutBase = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// lockoutSequences are attempt times (offsets from lockoutBase). They cover: the
// threshold inside one window, a window that elapses mid-run, a lock that expires,
// attempts while locked, and the boundary where a failure is exactly one window old.
func lockoutSequences() map[string][]time.Duration {
	secs := func(n int, step time.Duration) []time.Duration {
		out := make([]time.Duration, n)
		for i := range out {
			out[i] = time.Duration(i) * step
		}
		return out
	}
	return map[string][]time.Duration{
		"nine then a tenth in one window":     secs(10, time.Second),
		"attempts while locked keep the lock": secs(14, time.Second),
		"window elapses before the threshold": append(secs(9, time.Second), 9*time.Second+person.LockoutWindow+time.Second, 9*time.Second+person.LockoutWindow+2*time.Second),
		"exactly one window old is not elapsed": append(secs(9, time.Second),
			8*time.Second+person.LockoutWindow, 8*time.Second+person.LockoutWindow+time.Second),
		"lock expires then a fresh run": append(secs(10, time.Second),
			10*time.Second+person.LockoutDuration+person.LockoutWindow+time.Minute,
			10*time.Second+person.LockoutDuration+person.LockoutWindow+time.Minute+time.Second),
		"lock expires inside the window of the last failure": append(secs(10, time.Second),
			9*time.Second+person.LockoutDuration+time.Second, 9*time.Second+person.LockoutDuration+2*time.Second),
		"a single attempt": secs(1, time.Second),
	}
}

// step is what one attempt produced, comparable across implementations.
type step struct {
	count      int
	locked     time.Time
	justLocked bool
}

func aggregateRun(t *testing.T, addr string, offsets []time.Duration) []step {
	t.Helper()
	p := newPerson(t, addr)
	out := make([]step, 0, len(offsets))
	for _, off := range offsets {
		now := lockoutBase.Add(off)
		was := p.IsLocked(now)
		p.RegisterFailedLogin(now)
		out = append(out, step{p.FailedLoginCount(), p.LockedUntil(), !was && p.IsLocked(now)})
	}
	return out
}

// The persons statement and the Person aggregate agree on every sequence. The
// statement is a second implementation of the policy the aggregate states; this is
// what stops them drifting.
//
// arch-test:parallel-safe — shared pgtest container; every case owns a unique email (person row or
// phantom key), so rows never overlap; no TruncateAll, no process-global state.
func TestPersonRepository_UpdateAfterFailedLogin_MatchesTheAggregatePolicy(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	for name, offsets := range lockoutSequences() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			addr := "lockout-" + ids.NewV7().String()[24:] + "@example.test"
			p := newPerson(t, addr)
			if err := repo.Add(t.Context(), p); err != nil {
				t.Fatalf("Add: %v", err)
			}
			want := aggregateRun(t, "oracle-"+addr, offsets)
			for i, off := range offsets {
				out, err := repo.UpdateAfterFailedLogin(t.Context(), p.ID(), lockoutBase.Add(off))
				if err != nil {
					t.Fatalf("attempt %d: %v", i+1, err)
				}
				got := step{out.FailedCount, out.LockedUntil, out.JustLocked}
				if got.count != want[i].count || !got.locked.Equal(want[i].locked) || got.justLocked != want[i].justLocked {
					t.Fatalf("attempt %d at +%v: statement %+v, aggregate %+v", i+1, off, got, want[i])
				}
			}
		})
	}
}

// The phantom statement follows the same policy as well.
//
// arch-test:parallel-safe — shared pgtest container; every case owns a unique email (person row or
// phantom key), so rows never overlap; no TruncateAll, no process-global state.
func TestPhantomLockoutRepository_RegisterFailure_MatchesTheAggregatePolicy(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewPhantomLockoutRepository(pool, pg.NewTransactor(pool), []byte("phantom-parity-secret-0123456789"))
	for name, offsets := range lockoutSequences() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			addr, err := email.New("phantom-" + ids.NewV7().String()[24:] + "@example.test")
			if err != nil {
				t.Fatal(err)
			}
			want := aggregateRun(t, "oracle-"+addr.String(), offsets)
			for i, off := range offsets {
				out, err := repo.RegisterFailure(t.Context(), addr, lockoutBase.Add(off))
				if err != nil {
					t.Fatalf("attempt %d: %v", i+1, err)
				}
				if out.FailedCount != want[i].count || !out.LockedUntil.Equal(want[i].locked) {
					t.Fatalf("attempt %d at +%v: statement count=%d locked=%v, aggregate count=%d locked=%v",
						i+1, off, out.FailedCount, out.LockedUntil, want[i].count, want[i].locked)
				}
			}
		})
	}
}

// Concurrent attempts on one row are serialised by the statement: counts 1..N each
// seen exactly once (no lost update), the lock is crossed by exactly one attempt,
// and everything after it reports the same lock.
//
// arch-test:parallel-safe — shared pgtest container; every case owns a unique email (person row or
// phantom key), so rows never overlap; no TruncateAll, no process-global state.
func TestUpdateAfterFailedLogin_ConcurrentAttemptsAreSerialisedOnTheRow(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	persons := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	phantom := adapters.NewPhantomLockoutRepository(pool, pg.NewTransactor(pool), []byte("phantom-conc-secret-0123456789ab"))
	p := newPerson(t, "conc-"+ids.NewV7().String()[24:]+"@example.test")
	if err := persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	ghost, _ := email.New("ghost-" + ids.NewV7().String()[24:] + "@example.test")

	const n = 25
	for name, register := range map[string]func() (person.LockoutOutcome, error){
		"real": func() (person.LockoutOutcome, error) {
			return persons.UpdateAfterFailedLogin(t.Context(), p.ID(), lockoutBase)
		},
		"phantom": func() (person.LockoutOutcome, error) {
			return phantom.RegisterFailure(t.Context(), ghost, lockoutBase)
		},
	} {
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			outs []person.LockoutOutcome
		)
		start := make(chan struct{})
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				o, err := register()
				if err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
				mu.Lock()
				outs = append(outs, o)
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		seen := map[int]int{}
		locks := 0
		var until time.Time
		for _, o := range outs {
			seen[o.FailedCount]++
			if o.JustLocked {
				locks++
			}
			if o.LockedAt(lockoutBase) {
				if until.IsZero() {
					until = o.LockedUntil
				} else if !until.Equal(o.LockedUntil) {
					t.Errorf("%s: two different locks %v and %v", name, until, o.LockedUntil)
				}
			}
		}
		for c := 1; c <= n; c++ {
			if seen[c] != 1 {
				t.Errorf("%s: count %d seen %d times (a lost or duplicated update)", name, c, seen[c])
			}
		}
		if name == "real" && locks != 1 {
			t.Errorf("real: %d attempts reported crossing the threshold, want exactly 1", locks)
		}
		lockedAnswers := 0
		for _, o := range outs {
			if o.LockedAt(lockoutBase) {
				lockedAnswers++
			}
		}
		if want := n - person.MaxFailedLogins + 1; lockedAnswers != want {
			t.Errorf("%s: %d locked answers, want %d (the crossing attempt and everything after it)", name, lockedAnswers, want)
		}
	}
}

// The attempt that crosses the threshold records the account-locked event in the
// same transaction, once; attempts before and after record none.
//
// arch-test:no-parallel — subscriber-fixture test; uses TruncateAll
// (strict-TDL outbox-observation rule per ADR 0062 Amendment 1).
func TestPersonRepository_UpdateAfterFailedLogin_RecordsTheLockEventOnce(t *testing.T) {
	sharedPG.TruncateAll(t)
	fix := newOutboxFixture(t)
	repo := adapters.NewPersonRepository(fix.pool, pg.NewTransactor(fix.pool))
	p := newPerson(t, "lock-event@example.test")
	if err := repo.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	for i := range person.MaxFailedLogins + 3 {
		if _, err := repo.UpdateAfterFailedLogin(t.Context(), p.ID(), lockoutBase.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	got := eventTypes(fix.forwardAndWait(t, 2)) // person_created + exactly one account_locked
	locked := 0
	for _, et := range got {
		if et == "identity.person_account_locked.v1" {
			locked++
		}
	}
	if locked != 1 {
		t.Fatalf("event types %v: want exactly one identity.person_account_locked.v1", got)
	}
}

// arch-test:parallel-safe — shared pgtest container; every case owns a unique email (person row or
// phantom key), so rows never overlap; no TruncateAll, no process-global state.
func TestPersonRepository_UpdateAfterFailedLogin_UnknownPersonIsNotFound(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	_, err := repo.UpdateAfterFailedLogin(t.Context(), person.ID(ids.NewV7().String()), lockoutBase)
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("err = %v, want person.ErrNotFound", err)
	}
}

// What the phantom table holds: a 32-byte HMAC, never the address, never a plain
// or unsalted hash of it, and a different server secret gives a different key.
//
// arch-test:parallel-safe — shared pgtest container; every case owns a unique email (person row or
// phantom key), so rows never overlap; no TruncateAll, no process-global state.
func TestPhantomLockoutRepository_StoresNoEmailOnlyAnHMAC(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	local := "victim-" + ids.NewV7().String()[24:]
	addr, _ := email.New(local + "@example.test")
	a := adapters.NewPhantomLockoutRepository(pool, tx, []byte("server-secret-A-0123456789abcdef"))
	b := adapters.NewPhantomLockoutRepository(pool, tx, []byte("server-secret-B-0123456789abcdef"))
	for _, r := range []*adapters.PhantomLockoutRepository{a, b} {
		if _, err := r.RegisterFailure(t.Context(), addr, lockoutBase); err != nil {
			t.Fatal(err)
		}
	}
	// Both repositories counted the SAME address under their own key: two rows.
	rows, err := pool.Query(t.Context(), `SELECT key, row_to_json(t)::text FROM identity.login_phantom_failures t
		WHERE last_failed_at = $1 AND failed_count = 1`, lockoutBase)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plainSHA := sha256.Sum256([]byte(addr.String()))
	var keys [][]byte
	for rows.Next() {
		var key []byte
		var js string
		if err := rows.Scan(&key, &js); err != nil {
			t.Fatal(err)
		}
		if len(key) != 32 {
			t.Fatalf("key is %d bytes, want an HMAC-SHA-256 (32)", len(key))
		}
		if string(key) == string(plainSHA[:]) || string(key) == addr.String() {
			t.Fatal("the key is the address or an unsalted SHA-256 of it")
		}
		if strings.Contains(strings.ToLower(js), local) {
			t.Fatalf("the row mentions the address: %s", js)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The table is shared with parallel tests; the two keys of THIS address are
	// among the rows and differ from each other.
	var mine [][]byte
	for _, k := range keys {
		if a.KeyForTest(addr) == string(k) || b.KeyForTest(addr) == string(k) {
			mine = append(mine, k)
		}
	}
	if len(mine) != 2 || string(mine[0]) == string(mine[1]) {
		t.Fatalf("expected two distinct keys for one address under two secrets, got %d", len(mine))
	}
}

// The purge deletes rows whose window AND lock have both passed, only those, and
// no more than the batch size per call.
//
// arch-test:no-parallel — purges the shared table, so it must not overlap other
// tests' rows; uses TruncateAll.
func TestPhantomLockoutRepository_PurgeExpired_DeletesOnlyDeadRowsInBatches(t *testing.T) {
	sharedPG.TruncateAll(t)
	pool := repoFixture(t)
	repo := adapters.NewPhantomLockoutRepository(pool, pg.NewTransactor(pool), []byte("phantom-purge-secret-0123456789a"))
	mk := func(name string) email.Address {
		a, _ := email.New(name + "@example.test")
		return a
	}
	now := lockoutBase.Add(time.Hour)
	old := mk("old-unlocked") // one failure long ago: dead
	oldA, oldB := mk("old-unlocked-2"), mk("old-unlocked-3")
	fresh := mk("fresh")        // one failure a minute ago: window still open
	lockedStill := mk("locked") // locked until after now: must stay
	for _, a := range []email.Address{old, oldA, oldB} {
		if _, err := repo.RegisterFailure(t.Context(), a, now.Add(-person.LockoutWindow-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.RegisterFailure(t.Context(), fresh, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	for range person.MaxFailedLogins {
		if _, err := repo.RegisterFailure(t.Context(), lockedStill, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// An expired lock whose window also passed is dead too.
	expiredLock := mk("expired-lock")
	for range person.MaxFailedLogins {
		if _, err := repo.RegisterFailure(t.Context(), expiredLock, now.Add(-person.LockoutDuration-person.LockoutWindow-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	n, err := repo.PurgeExpired(t.Context(), now, 2)
	if err != nil || n != 2 {
		t.Fatalf("first purge deleted %d (err %v), want exactly the batch of 2", n, err)
	}
	n, err = repo.PurgeExpired(t.Context(), now, 100)
	if err != nil || n != 2 {
		t.Fatalf("second purge deleted %d (err %v), want the remaining 2 dead rows", n, err)
	}
	n, err = repo.PurgeExpired(t.Context(), now, 100)
	if err != nil || n != 0 {
		t.Fatalf("third purge deleted %d (err %v), want 0", n, err)
	}
	if out, err := repo.RegisterFailure(t.Context(), lockedStill, now); err != nil || !out.LockedAt(now) {
		t.Fatalf("an active lock was purged: outcome=%+v err=%v", out, err)
	}
	var left int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM identity.login_phantom_failures`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("%d rows left (err %v), want fresh + locked", left, err)
	}
}
