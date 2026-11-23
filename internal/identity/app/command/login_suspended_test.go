package command_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// newPersonSuspended is a Person under a global suspension (the operator's ban).
// The credential is intact and the person stays "active": the flag is the only
// thing that says no.
func newPersonSuspended(t *testing.T, plain string) *person.Person {
	t.Helper()
	live := newPersonWithPassword(t, plain)
	return person.UnmarshalFromDB(person.Snapshot{
		ID: live.ID(), Email: live.Email(), FirstName: live.FirstName(), LastName: live.LastName(),
		PasswordHash: live.PasswordHash(), SecurityStamp: live.SecurityStamp(),
		IsActive: true, IsGloballySuspended: true, GlobalSuspensionReason: "fraud review",
		GloballySuspendedAt: testNow, CreatedAt: live.CreatedAt(),
	})
}

// Fix round 13, item 1: a globally suspended person's CORRECT password must not
// sign them in. Before the fix the pre-verify gate read only active, so
// the ban lasted until the next login. The answer is the unknown-email answer: 401,
// no family, and the person's own counter never moves.
func TestLogin_SuspendedPerson_CorrectPassword_IsRefusedWithoutASession(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "correct horse battery staple"
	p := newPersonSuspended(t, plain)
	tn := newTenantWithSlug(t, "acme")
	m := activeMembership(t, p, tn)
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}

	_, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "banned"})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("suspended person, correct password = %v, want ErrInvalidCredentials", err)
	}
	fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(fams) != 0 {
		t.Fatalf("%d refresh families for a suspended person, want 0", len(fams))
	}
	if p.FailedLoginCount() != 0 {
		t.Fatalf("a suspended person's real counter moved to %d", p.FailedLoginCount())
	}
}

// Lifting the suspension restores sign-in (the flag is the only gate).
func TestLogin_SuspensionLifted_SignsInAgain(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "correct horse battery staple"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	m := activeMembership(t, p, tn)
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	if err := rig.persons.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) {
		return true, loaded.GloballySuspend("review", testNow)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "d"}); !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("while suspended: %v, want ErrInvalidCredentials", err)
	}
	if err := rig.persons.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) {
		return true, loaded.LiftGlobalSuspension(testNow)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "d"}); err != nil {
		t.Fatalf("after the lift: %v, want a session", err)
	}
}

// openSession's re-read under the SHARE lock refuses a person who stopped being
// allowed to sign in between the verify and the family insert. One case per
// terminal state, so removing any single term of the predicate fails a test (fix
// round 13: the surviving mutants of the round-12 review).
func TestLogin_PersonBecomesTerminalBetweenVerifyAndInsert_IsRefused(t *testing.T) {
	t.Parallel()
	for name, becomeTerminal := range map[string]func(t *testing.T, loaded *person.Person) error{
		"globally suspended": func(_ *testing.T, loaded *person.Person) error {
			return loaded.GloballySuspend("fraud", testNow)
		},
		"deactivated": func(t *testing.T, loaded *person.Person) error {
			t.Helper()
			*loaded = *person.UnmarshalFromDB(person.Snapshot{
				ID: loaded.ID(), Email: loaded.Email(), FirstName: loaded.FirstName(), LastName: loaded.LastName(),
				PasswordHash: loaded.PasswordHash(), SecurityStamp: loaded.SecurityStamp(),
				IsActive: false, CreatedAt: loaded.CreatedAt(),
			})
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newLoginRig(t)
			const plain = "correct horse battery staple"
			p := newPersonWithPassword(t, plain)
			tn := newTenantWithSlug(t, "acme")
			m := activeMembership(t, p, tn)
			seedLoginCandidate(t, rig, p, m)
			if err := rig.tenants.Add(t.Context(), tn); err != nil {
				t.Fatalf("tenants.Add: %v", err)
			}
			realHash := p.PasswordHash().String()

			var fired atomic.Bool
			h := rig.handler.WithPasswordVerifier(func(password, encoded string) error {
				verr := argon2.Verify(password, encoded)
				if verr == nil && encoded == realHash && fired.CompareAndSwap(false, true) {
					if err := rig.persons.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) {
						return true, becomeTerminal(t, loaded)
					}); err != nil {
						t.Errorf("state change in the gap: %v", err)
					}
				}
				return verr
			})
			_, err := h.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "gap"})
			if !errors.Is(err, command.ErrInvalidCredentials) {
				t.Fatalf("%s between verify and insert: %v, want ErrInvalidCredentials", name, err)
			}
			fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
			if err != nil {
				t.Fatal(err)
			}
			if len(fams) != 0 {
				t.Fatalf("%d active families after a refused login, want 0", len(fams))
			}
		})
	}
}

// Fix round 13, item 8: the membership and the tenant are re-read INSIDE the
// session transaction (after the person's SHARE lock). A membership deactivated, or
// a tenant suspended, between the resolve and the family insert yields no session.
func TestLogin_MembershipOrTenantChangesBetweenVerifyAndInsert_IsRefused(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(rig wiredLoginRig, m *membership.Membership) error{
		"membership deactivated": func(_ wiredLoginRig, m *membership.Membership) error {
			return m.Deactivate("offboarded", testNow)
		},
		"tenant suspended": func(rig wiredLoginRig, _ *membership.Membership) error {
			rig.router.tenantStatus = tenant.StatusSuspended
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newLoginRig(t)
			const plain = "correct horse battery staple"
			p := newPersonWithPassword(t, plain)
			tn := newTenantWithSlug(t, "acme")
			m := activeMembership(t, p, tn)
			seedLoginCandidate(t, rig, p, m)
			if err := rig.tenants.Add(t.Context(), tn); err != nil {
				t.Fatalf("tenants.Add: %v", err)
			}
			realHash := p.PasswordHash().String()
			var fired atomic.Bool
			h := rig.handler.WithPasswordVerifier(func(password, encoded string) error {
				verr := argon2.Verify(password, encoded)
				if verr == nil && encoded == realHash && fired.CompareAndSwap(false, true) {
					if err := change(rig, m); err != nil {
						t.Errorf("change in the gap: %v", err)
					}
				}
				return verr
			})
			_, err := h.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "gap"})
			if !errors.Is(err, command.ErrInvalidCredentials) {
				t.Fatalf("%s between verify and insert: %v, want ErrInvalidCredentials", name, err)
			}
			fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
			if err != nil {
				t.Fatal(err)
			}
			if len(fams) != 0 {
				t.Fatalf("%d active families after a refused login, want 0", len(fams))
			}
		})
	}
}
