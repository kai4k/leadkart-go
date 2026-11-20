//go:build integration

// arch-test:no-timeout-needed — shares the package's pgtest container; execution
//   is bounded by the pgxpool conn timeouts and the package-level test timeout.
//
// arch-test:parallel-safe — every test registers its own tenant and person with
//   a unique email, and the phantom counter is keyed per address.

package command_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// concurrentWrongLogins fires n wrong-password logins for addr at once (a barrier
// holds them until all are ready) and returns how many of each answer came back.
func concurrentWrongLogins(t *testing.T, h command.LoginHandler, addr email.Address, n int) map[outcome]int {
	t.Helper()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = map[outcome]int{}
		start   = make(chan struct{})
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: "WRONG-concurrent", DeviceLabel: "t"})
			var o outcome
			switch {
			case errors.Is(err, command.ErrAccountLocked):
				o = outcomeLocked
			case errors.Is(err, command.ErrInvalidCredentials):
				o = outcomeInvalid
			default:
				t.Errorf("unexpected answer: %v", err)
				return
			}
			mu.Lock()
			results[o]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	return results
}

// registerRealAccount creates a tenant + admin and returns the admin's address.
func registerRealAccount(t *testing.T, app wiredApp) email.Address {
	t.Helper()
	full := ids.NewV7().String()
	s, err := slug.New("conc-" + full[len(full)-8:])
	if err != nil {
		t.Fatal(err)
	}
	addr, err := email.New("conc-" + full[len(full)-8:] + "@flow.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.register.Handle(t.Context(), command.RegisterTenantCommand{
		Slug: s, LegalName: "Conc Pharma Pvt Ltd", DisplayName: "Conc", AdminEmail: addr,
		AdminPassword: "correct horse battery staple", AdminFirstName: "A", AdminLastName: "B",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return addr
}

// ADR 0068 D12: ten simultaneous wrong logins are an existence oracle if the real
// account's counter loses updates (ten 401s) while the unknown address's counter
// is serialised (one 423). Both counters are now one atomic statement, so a real
// and an unknown address answer with the SAME multiset: nine 401s and exactly one
// 423, and a real account's stored count is exactly ten (nothing lost).
func TestLogin_ConcurrentWrongPasswords_RealAndUnknownAddressesAnswerIdentically(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	realAddr := registerRealAccount(t, app)
	unknownAddr, _ := email.New("nobody-" + ids.NewV7().String()[24:] + "@flow.test")

	want := map[outcome]int{outcomeInvalid: person.MaxFailedLogins - 1, outcomeLocked: 1}
	gotReal := concurrentWrongLogins(t, app.login, realAddr, person.MaxFailedLogins)
	gotUnknown := concurrentWrongLogins(t, app.login, unknownAddr, person.MaxFailedLogins)
	if !equalCounts(gotReal, want) {
		t.Errorf("real account: %v, want %v", gotReal, want)
	}
	if !equalCounts(gotUnknown, want) {
		t.Errorf("unknown address: %v, want %v", gotUnknown, want)
	}

	persons := adapters.NewPersonRepository(app.pool, pg.NewTransactor(app.pool))
	p, err := persons.GetByEmail(t.Context(), realAddr)
	if err != nil {
		t.Fatal(err)
	}
	if p.FailedLoginCount() != person.MaxFailedLogins {
		t.Fatalf("stored failed count = %d, want exactly %d (a lost update)", p.FailedLoginCount(), person.MaxFailedLogins)
	}

	// A batch that arrives after the lock is refused whole, on both.
	for name, addr := range map[string]email.Address{"real": realAddr, "unknown": unknownAddr} {
		got := concurrentWrongLogins(t, app.login, addr, 5)
		if !equalCounts(got, map[outcome]int{outcomeLocked: 5}) {
			t.Errorf("%s after the lock: %v, want five 423s", name, got)
		}
	}
	// ...and the CORRECT password stays refused while locked.
	_, err = app.login.Handle(t.Context(), command.LoginCommand{Email: realAddr, Password: "correct horse battery staple", DeviceLabel: "t"})
	if !errors.Is(err, command.ErrAccountLocked) {
		t.Fatalf("correct password while locked: %v, want ErrAccountLocked", err)
	}
}

// Many more simultaneous attempts than the threshold still lock exactly once and
// never let a batch slip a guess past the lock: with 40 at once, the number of
// plain 401s is exactly nine on both paths (the rest, however they interleave, are
// 423s).
func TestLogin_ABatchLargerThanTheThresholdCannotGuessPastTheLock(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	realAddr := registerRealAccount(t, app)
	unknownAddr, _ := email.New("nobody-" + ids.NewV7().String()[24:] + "@flow.test")
	const n = 40
	for name, addr := range map[string]email.Address{"real": realAddr, "unknown": unknownAddr} {
		got := concurrentWrongLogins(t, app.login, addr, n)
		if got[outcomeInvalid] != person.MaxFailedLogins-1 || got[outcomeLocked] != n-(person.MaxFailedLogins-1) {
			t.Errorf("%s: %v, want %d x 401 and %d x 423", name, got, person.MaxFailedLogins-1, n-(person.MaxFailedLogins-1))
		}
	}
}

func equalCounts(a, b map[outcome]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
