package person_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// Lockout-policy + must-change-password tests per ADR 0053. Pure
// aggregate-level coverage — repository / login-flow integration
// tests live elsewhere.

// ----- RegisterFailedLogin --------------------------------------------------

func TestPerson_RegisterFailedLogin_BelowThreshold_OnlyIncrements(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()

	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	for i := 1; i < person.MaxFailedLogins; i++ {
		p.RegisterFailedLogin(now.Add(time.Duration(i) * time.Second))
		if got := p.FailedLoginCount(); got != i {
			t.Errorf("after attempt %d: FailedLoginCount = %d, want %d", i, got, i)
		}
		if p.IsLocked(now.Add(time.Hour)) {
			t.Errorf("attempt %d: account should NOT be locked yet", i)
		}
	}
	if evs := p.PullEvents(); len(evs) != 0 {
		t.Errorf("expected 0 lock events below threshold, got %d", len(evs))
	}
}

func TestPerson_RegisterFailedLogin_AtThreshold_Locks(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()

	base := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	var lastNow time.Time
	for i := 0; i < person.MaxFailedLogins; i++ {
		lastNow = base.Add(time.Duration(i) * time.Second)
		p.RegisterFailedLogin(lastNow)
	}
	if !p.IsLocked(lastNow.Add(time.Second)) {
		t.Fatalf("account should be locked at threshold")
	}
	wantUntil := lastNow.Add(person.LockoutDuration)
	if got := p.LockedUntil(); !got.Equal(wantUntil) {
		t.Errorf("LockedUntil = %v, want %v", got, wantUntil)
	}

	events := p.PullEvents()
	var locked *person.AccountLockedEvent
	for i := range events {
		if le, ok := events[i].(person.AccountLockedEvent); ok {
			locked = &le
			break
		}
	}
	if locked == nil {
		t.Fatal("expected PersonAccountLockedEvent")
	}
	if locked.FailedCount != person.MaxFailedLogins {
		t.Errorf("locked.FailedCount = %d, want %d", locked.FailedCount, person.MaxFailedLogins)
	}
	if !locked.LockedUntil.Equal(wantUntil) {
		t.Errorf("locked.LockedUntil = %v, want %v", locked.LockedUntil, wantUntil)
	}
}

func TestPerson_RegisterFailedLogin_OutsideWindow_ResetsCounter(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()

	t0 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	// Build up some failures inside the first window.
	for i := 0; i < 3; i++ {
		p.RegisterFailedLogin(t0.Add(time.Duration(i) * time.Second))
	}
	if got := p.FailedLoginCount(); got != 3 {
		t.Fatalf("setup: FailedLoginCount = %d, want 3", got)
	}

	// Next attempt arrives well outside the sliding window — counter
	// MUST reset to 1, NOT increment to 4.
	outside := t0.Add(person.LockoutWindow + time.Minute)
	p.RegisterFailedLogin(outside)
	if got := p.FailedLoginCount(); got != 1 {
		t.Errorf("after outside-window attempt: FailedLoginCount = %d, want 1", got)
	}
	if p.IsLocked(outside.Add(time.Second)) {
		t.Errorf("account should NOT be locked after window-reset attempt")
	}
}

// ----- RegisterSuccessfulLogin ----------------------------------------------

func TestPerson_RegisterSuccessfulLogin_ClearsCounter(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()

	t0 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		p.RegisterFailedLogin(t0.Add(time.Duration(i) * time.Second))
	}
	_ = p.PullEvents() // drain any incidental events

	p.RegisterSuccessfulLogin()
	if got := p.FailedLoginCount(); got != 0 {
		t.Errorf("FailedLoginCount = %d, want 0", got)
	}
	if !p.LockedUntil().IsZero() {
		t.Errorf("LockedUntil should be zero, got %v", p.LockedUntil())
	}
	if !p.LastFailedLoginAt().IsZero() {
		t.Errorf("LastFailedLoginAt should be zero, got %v", p.LastFailedLoginAt())
	}
}

func TestPerson_RegisterSuccessfulLogin_AfterFailures_EmitsUnlockEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 23, 13, 0, 0, 0, time.UTC))
	p := newPerson(t)
	_ = p.PullEvents()

	p.RegisterFailedLogin(time.Date(2026, 5, 23, 12, 30, 0, 0, time.UTC))
	_ = p.PullEvents()

	p.RegisterSuccessfulLogin()
	events := p.PullEvents()
	var found bool
	for _, e := range events {
		if _, ok := e.(person.AccountUnlockedEvent); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected PersonAccountUnlockedEvent, got %v", events)
	}
}

func TestPerson_RegisterSuccessfulLogin_Clean_NoEvent(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()

	// No prior failures — successful login should be a silent no-op
	// (clean accounts don't emit unlock events).
	p.RegisterSuccessfulLogin()
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on clean success, got %d", len(got))
	}
}

// ----- IsLocked --------------------------------------------------------------

func TestPerson_IsLocked_RespectsLockedUntil(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()

	base := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	for i := 0; i < person.MaxFailedLogins; i++ {
		p.RegisterFailedLogin(base.Add(time.Duration(i) * time.Second))
	}
	lockedAt := base.Add(time.Duration(person.MaxFailedLogins-1) * time.Second)

	// While inside the lockout window — IsLocked returns true.
	if !p.IsLocked(lockedAt.Add(time.Minute)) {
		t.Errorf("IsLocked = false during lockout window")
	}
	// At exactly LockedUntil — boundary uses Before(), so equal time = NOT locked.
	if p.IsLocked(p.LockedUntil()) {
		t.Errorf("IsLocked = true at exact LockedUntil boundary")
	}
	// After LockedUntil — IsLocked returns false.
	if p.IsLocked(p.LockedUntil().Add(time.Second)) {
		t.Errorf("IsLocked = true after lockout expiry")
	}
}

func TestPerson_IsLocked_ZeroLockedUntil_IsNotLocked(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	if p.IsLocked(time.Now()) {
		t.Errorf("fresh Person should not be locked")
	}
}

// ----- MustChangePassword (BRD line 241) ------------------------------------

func TestPerson_NewWithMustChangePassword_SetsFlag(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))

	id := newID(t)
	p, err := person.NewWithMustChangePassword(id, mustEmail(t, "x@y.io"), "X", "Y", mustHash(t))
	if err != nil {
		t.Fatalf("NewWithMustChangePassword: %v", err)
	}
	if !p.MustChangePassword() {
		t.Error("MustChangePassword should be true on operator-provisioned factory")
	}
}

func TestPerson_New_DefaultsMustChangePasswordFalse(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	p := newPerson(t)
	if p.MustChangePassword() {
		t.Error("MustChangePassword should default false for self-rotated path")
	}
}

func TestPerson_ChangePassword_ClearsMustChangePassword(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))

	p, err := person.NewWithMustChangePassword(newID(t), mustEmail(t, "z@q.io"), "Z", "Q", mustHash(t))
	if err != nil {
		t.Fatalf("NewWithMustChangePassword: %v", err)
	}
	_ = p.PullEvents()
	if !p.MustChangePassword() {
		t.Fatal("setup: flag should be true")
	}

	newHash, err := person.NewPasswordHash(
		"$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdHNhbHRzYWx0$bmV3aGFzaG5ld2hhc2huZXc",
	)
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	if err := p.ChangePassword(newHash); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if p.MustChangePassword() {
		t.Error("ChangePassword should clear MustChangePassword")
	}
}

func TestPerson_ConfirmPasswordReset_ClearsMustChangePassword(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))

	p, err := person.NewWithMustChangePassword(newID(t), mustEmail(t, "r@s.io"), "R", "S", mustHash(t))
	if err != nil {
		t.Fatalf("NewWithMustChangePassword: %v", err)
	}
	tokenHash := mustResetHash(t, validResetHash)
	if err := p.RequestPasswordReset(tokenHash, time.Hour); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	_ = p.PullEvents()

	if !p.MustChangePassword() {
		t.Fatal("setup: flag should be true before confirm")
	}

	newHash, err := person.NewPasswordHash(
		"$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdHNhbHRzYWx0$bmV3aGFzaG5ld2hhc2huZXc",
	)
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	if err := p.ConfirmPasswordReset(tokenHash, newHash); err != nil {
		t.Fatalf("ConfirmPasswordReset: %v", err)
	}
	if p.MustChangePassword() {
		t.Error("ConfirmPasswordReset should clear MustChangePassword")
	}
}

func TestPerson_MarkPasswordChanged_ClearsFlag_NoEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))

	p, err := person.NewWithMustChangePassword(newID(t), mustEmail(t, "m@n.io"), "M", "N", mustHash(t))
	if err != nil {
		t.Fatalf("NewWithMustChangePassword: %v", err)
	}
	_ = p.PullEvents()

	p.MarkPasswordChanged()
	if p.MustChangePassword() {
		t.Error("MarkPasswordChanged should clear flag")
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("MarkPasswordChanged should NOT emit events, got %d", len(got))
	}
	// Idempotent.
	p.MarkPasswordChanged()
}
