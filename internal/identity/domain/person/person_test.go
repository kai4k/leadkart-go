package person_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func mustEmail(t *testing.T, raw string) email.Address {
	t.Helper()
	e, err := email.New(raw)
	if err != nil {
		t.Fatalf("mustEmail(%q): %v", raw, err)
	}
	return e
}

func mustHash(t *testing.T) person.PasswordHash {
	t.Helper()
	h, err := person.NewPasswordHash(
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
	)
	if err != nil {
		t.Fatalf("mustHash: %v", err)
	}
	return h
}

func newID(t *testing.T) person.ID {
	t.Helper()
	return person.ID(ids.NewV7().String())
}

// ----- Factory: New ---------------------------------------------------------

func TestNewPerson_AcceptsValid(t *testing.T) {
	t.Parallel()

	id := newID(t)
	e := mustEmail(t, "alice@example.com")
	p, err := person.New(id, e, "Alice", "Sharma", mustHash(t), testNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("New returned nil")
	}
	if p.ID() != id {
		t.Errorf("ID() = %v, want %v", p.ID(), id)
	}
	if !p.Email().Equal(e) {
		t.Errorf("Email mismatch")
	}
	if p.FirstName() != "Alice" {
		t.Errorf("FirstName() = %q", p.FirstName())
	}
	if p.LastName() != "Sharma" {
		t.Errorf("LastName() = %q", p.LastName())
	}
	if p.PasswordHash().IsZero() {
		t.Error("password hash not set")
	}
	if p.SecurityStamp().IsZero() {
		t.Error("security stamp not set on creation")
	}
	if !p.IsActive() {
		t.Error("new Person should default to IsActive=true")
	}
	if p.IsAnonymised() {
		t.Error("new Person should not be anonymised")
	}
	if !p.CreatedAt().Equal(testNow) {
		t.Errorf("CreatedAt = %v", p.CreatedAt())
	}
}

func TestNewPerson_EmitsCreatedEvent(t *testing.T) {
	t.Parallel()

	id := newID(t)
	p, err := person.New(id, mustEmail(t, "a@b.io"), "A", "B", mustHash(t), testNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	created, ok := events[0].(person.CreatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want CreatedEvent", events[0])
	}
	if created.PersonID != id {
		t.Errorf("event PersonID = %v, want %v", created.PersonID, id)
	}
}

func TestNewPerson_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		id        person.ID
		email     email.Address
		firstName string
		lastName  string
		hash      person.PasswordHash
	}{
		{"zero id", person.ID(""), mustEmail(t, "a@b.io"), "A", "B", mustHash(t)},
		{"zero email", person.ID(ids.NewV7().String()), email.Address{}, "A", "B", mustHash(t)},
		{"empty first name", person.ID(ids.NewV7().String()), mustEmail(t, "a@b.io"), "", "B", mustHash(t)},
		{"empty last name", person.ID(ids.NewV7().String()), mustEmail(t, "a@b.io"), "A", "", mustHash(t)},
		{"zero hash", person.ID(ids.NewV7().String()), mustEmail(t, "a@b.io"), "A", "B", person.PasswordHash{}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := person.New(tc.id, tc.email, tc.firstName, tc.lastName, tc.hash, testNow)
			if err == nil {
				t.Fatalf("expected error")
			}
			if errs.KindOf(err) != errs.KindInvalidInput {
				t.Errorf("Kind = %v, want KindInvalidInput", errs.KindOf(err))
			}
			if !errors.Is(err, person.ErrInvalid) {
				t.Errorf("expected errors.Is(_, ErrInvalid)")
			}
		})
	}
}

// ----- ChangePassword -------------------------------------------------------

func TestChangePassword_RotatesSecurityStamp(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	_ = p.PullEvents()
	originalStamp := p.SecurityStamp()

	newHash, _ := person.NewPasswordHash(
		"$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdG5ld3NhbHQ$bmV3aGFzaG5ld2hhc2huZXc",
	)
	if err := p.ChangePassword(newHash, testNow); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	if p.SecurityStamp().Equal(originalStamp) {
		t.Fatal("SecurityStamp did not rotate after password change")
	}
	if !p.PasswordHash().IsZero() && p.PasswordHash().String() != newHash.String() {
		t.Errorf("password hash not updated")
	}

	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(person.PasswordChangedEvent); !ok {
		t.Errorf("event[0] = %T, want PasswordChangedEvent", events[0])
	}
}

func TestChangePassword_RejectsZeroHash(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	if err := p.ChangePassword(person.PasswordHash{}, testNow); err == nil {
		t.Fatal("expected error on zero hash")
	}
}

// ----- UpdateProfile --------------------------------------------------------

func TestUpdateProfile_ChangesNameAndEmitsEvent(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	_ = p.PullEvents() // drain CreatedEvent

	if err := p.UpdateProfile("Alice", "Sharma-Khan", testNow); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if p.FirstName() != "Alice" {
		t.Errorf("FirstName = %q, want Alice", p.FirstName())
	}
	if p.LastName() != "Sharma-Khan" {
		t.Errorf("LastName = %q, want Sharma-Khan", p.LastName())
	}

	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev, ok := events[0].(person.ProfileUpdatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want ProfileUpdatedEvent", events[0])
	}
	if ev.OldFirstName != "A" || ev.OldLastName != "B" {
		t.Errorf("OLD = (%q, %q), want (A, B)", ev.OldFirstName, ev.OldLastName)
	}
	if ev.NewFirstName != "Alice" || ev.NewLastName != "Sharma-Khan" {
		t.Errorf("NEW = (%q, %q), want (Alice, Sharma-Khan)", ev.NewFirstName, ev.NewLastName)
	}
}

func TestUpdateProfile_NoOp_WhenNamesUnchanged(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()

	if err := p.UpdateProfile("A", "B", testNow); err != nil {
		t.Fatalf("UpdateProfile no-op: %v", err)
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on no-op update, got %d", len(got))
	}
}

func TestUpdateProfile_RejectsEmptyAndOverlong(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		firstName string
		lastName  string
	}{
		{"empty first", "", "B"},
		{"empty last", "A", ""},
		{"whitespace first", "   ", "B"},
		{"whitespace last", "A", "  "},
		// 101 chars > nameMaxLen (100)
		{"first too long", string(make([]byte, 101)), "B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPerson(t)
			err := p.UpdateProfile(tc.firstName, tc.lastName, testNow)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !errors.Is(err, person.ErrInvalid) {
				t.Errorf("expected errors.Is(_, ErrInvalid), got %v", err)
			}
		})
	}
}

func TestUpdateProfile_RefusedOnAnonymisedPerson(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.Anonymise(testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	err := p.UpdateProfile("Alice", "Sharma", testNow)
	if err == nil {
		t.Fatal("expected error updating anonymised person")
	}
	if !errors.Is(err, person.ErrInvalid) {
		t.Errorf("expected errors.Is(_, ErrInvalid), got %v", err)
	}
}

// ----- Password reset (token flow) ------------------------------------------

const validResetHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" // 64 hex chars

func mustResetHash(t *testing.T, raw string) person.PasswordResetTokenHash {
	t.Helper()
	h, err := person.NewPasswordResetTokenHash(raw)
	if err != nil {
		t.Fatalf("NewPasswordResetTokenHash(%q): %v", raw, err)
	}
	return h
}

func TestRequestPasswordReset_StoresPendingAndEmitsEvent(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	_ = p.PullEvents()

	h := mustResetHash(t, validResetHash)
	if err := p.RequestPasswordReset("plaintext", h, time.Hour, testNow); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	pending := p.PendingPasswordReset()
	if pending.IsZero() {
		t.Fatal("PendingPasswordReset is zero")
	}
	if !pending.Hash().Equal(h) {
		t.Errorf("hash mismatch")
	}
	expectedExpiry := testNow.Add(time.Hour)
	if !pending.ExpiresAt().Equal(expectedExpiry) {
		t.Errorf("ExpiresAt = %v, want %v", pending.ExpiresAt(), expectedExpiry)
	}

	events := p.PullEvents()
	// Per ADR 0057: TWO events fire — the AUDIT signal
	// (PasswordResetRequestedEvent, no plaintext) AND the ACTION
	// signal (PasswordResetEmailRequestedEvent, carries plaintext).
	if len(events) != 2 {
		t.Fatalf("events: %d, want 2", len(events))
	}
	ev, ok := events[0].(person.PasswordResetRequestedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want PasswordResetRequestedEvent", events[0])
	}
	if !ev.ExpiresAt.Equal(expectedExpiry) {
		t.Errorf("event ExpiresAt mismatch")
	}
	emailEv, ok := events[1].(person.PasswordResetEmailRequestedEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want PasswordResetEmailRequestedEvent", events[1])
	}
	if emailEv.PlaintextToken != "plaintext" {
		t.Errorf("dispatch plaintext = %q, want %q", emailEv.PlaintextToken, "plaintext")
	}
	if !emailEv.ExpiresAt.Equal(expectedExpiry) {
		t.Errorf("dispatch ExpiresAt mismatch")
	}
}

func TestRequestPasswordReset_RejectsZeroHashAndZeroTTL(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	if err := p.RequestPasswordReset("plaintext", person.PasswordResetTokenHash{}, time.Hour, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("zero hash: expected ErrInvalid, got %v", err)
	}
	h := mustResetHash(t, validResetHash)
	for _, ttl := range []time.Duration{0, -1 * time.Second} {
		if err := p.RequestPasswordReset("plaintext", h, ttl, testNow); !errors.Is(err, person.ErrInvalid) {
			t.Errorf("ttl %v: expected ErrInvalid, got %v", ttl, err)
		}
	}
}

func TestRequestPasswordReset_RejectedOnAnonymisedAndSuspended(t *testing.T) {
	t.Parallel()
	h := mustResetHash(t, validResetHash)

	pAnon := newPerson(t)
	_ = pAnon.Anonymise(testNow) // arch-test:ignore-err - test fixture setup
	if err := pAnon.RequestPasswordReset("plaintext", h, time.Hour, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("anonymised: expected ErrInvalid, got %v", err)
	}

	pSusp := newPerson(t)
	_ = pSusp.GloballySuspend("fraud", testNow) // arch-test:ignore-err - test fixture setup
	if err := pSusp.RequestPasswordReset("plaintext", h, time.Hour, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("suspended: expected ErrInvalid, got %v", err)
	}
}

func TestRequestPasswordReset_NewRequestSupersedesOld(t *testing.T) {
	t.Parallel()
	p := newPerson(t)

	first := mustResetHash(t, validResetHash)
	_ = p.RequestPasswordReset("plaintext-first", first, time.Hour, testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	second := mustResetHash(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	if err := p.RequestPasswordReset("plaintext-second", second, 30*time.Minute, testNow); err != nil {
		t.Fatalf("second request: %v", err)
	}
	pending := p.PendingPasswordReset()
	if !pending.Hash().Equal(second) {
		t.Error("second request did not supersede first")
	}
	expectedExpiry := testNow.Add(30 * time.Minute)
	if !pending.ExpiresAt().Equal(expectedExpiry) {
		t.Errorf("ExpiresAt = %v, want %v", pending.ExpiresAt(), expectedExpiry)
	}
}

func TestConfirmPasswordReset_AppliesNewPasswordAndRotatesStamp(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	originalStamp := p.SecurityStamp()
	originalHash := p.PasswordHash()

	h := mustResetHash(t, validResetHash)
	_ = p.RequestPasswordReset("plaintext", h, time.Hour, testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	newHash, _ := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdG5ld3NhbHQ$bmV3aGFzaG5ld2hhc2huZXc")
	if err := p.ConfirmPasswordReset(h, newHash, testNow); err != nil {
		t.Fatalf("ConfirmPasswordReset: %v", err)
	}
	if p.PendingPasswordReset().Hash().IsZero() != true {
		t.Error("pending reset not cleared after confirm")
	}
	if p.PasswordHash().String() == originalHash.String() {
		t.Error("PasswordHash not updated")
	}
	if p.SecurityStamp().Equal(originalStamp) {
		t.Error("SecurityStamp did not rotate")
	}

	events := p.PullEvents()
	if len(events) != 2 {
		t.Fatalf("events: %d, want 2 (Confirmed + Changed)", len(events))
	}
	if _, ok := events[0].(person.PasswordResetConfirmedEvent); !ok {
		t.Errorf("event[0] = %T, want PasswordResetConfirmedEvent", events[0])
	}
	if _, ok := events[1].(person.PasswordChangedEvent); !ok {
		t.Errorf("event[1] = %T, want PasswordChangedEvent", events[1])
	}
}

func TestConfirmPasswordReset_RejectsExpiredToken(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	h := mustResetHash(t, validResetHash)
	_ = p.RequestPasswordReset("plaintext", h, time.Hour, testNow) // arch-test:ignore-err - test fixture setup

	// Confirm AFTER the 1h TTL window — token expired.
	expired := testNow.Add(2 * time.Hour)
	newHash, _ := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdG5ld3NhbHQ$bmV3aGFzaG5ld2hhc2huZXc")
	if err := p.ConfirmPasswordReset(h, newHash, expired); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("expired: expected ErrInvalid, got %v", err)
	}
	// Pending defensively cleared after expired confirm — second call has no pending.
	if !p.PendingPasswordReset().IsZero() {
		t.Error("expired pending not cleared defensively")
	}
}

func TestConfirmPasswordReset_RejectsMismatchedHash(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	stored := mustResetHash(t, validResetHash)
	_ = p.RequestPasswordReset("plaintext", stored, time.Hour, testNow) // arch-test:ignore-err - test fixture setup

	wrong := mustResetHash(t, "0000000000000000000000000000000000000000000000000000000000000000")
	newHash, _ := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdG5ld3NhbHQ$bmV3aGFzaG5ld2hhc2huZXc")
	if err := p.ConfirmPasswordReset(wrong, newHash, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("wrong hash: expected ErrInvalid, got %v", err)
	}
	// Pending NOT cleared on mismatch — legitimate user retry within window must work.
	if p.PendingPasswordReset().IsZero() {
		t.Error("pending should remain after mismatch")
	}
}

func TestConfirmPasswordReset_RejectsWhenNoPending(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustResetHash(t, validResetHash)
	newHash, _ := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdG5ld3NhbHQ$bmV3aGFzaG5ld2hhc2huZXc")
	if err := p.ConfirmPasswordReset(h, newHash, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("no pending: expected ErrInvalid, got %v", err)
	}
}

func TestCancelPasswordReset_ClearsPendingAndEmitsEvent(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustResetHash(t, validResetHash)
	_ = p.RequestPasswordReset("plaintext", h, time.Hour, testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	if err := p.CancelPasswordReset("operator-cleared-stuck-reset", testNow); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !p.PendingPasswordReset().IsZero() {
		t.Error("pending not cleared")
	}
	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(person.PasswordResetCancelledEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want PasswordResetCancelledEvent", events[0])
	}
	if ev.Reason != "operator-cleared-stuck-reset" {
		t.Errorf("reason = %q", ev.Reason)
	}
}

func TestCancelPasswordReset_NoOp_WhenNoPending(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()
	if err := p.CancelPasswordReset("preemptive", testNow); err != nil {
		t.Fatalf("Cancel no-op: %v", err)
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on no-op cancel, got %d", len(got))
	}
}

func TestCancelPasswordReset_RequiresReason_WhenPending(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustResetHash(t, validResetHash)
	_ = p.RequestPasswordReset("plaintext", h, time.Hour, testNow) // arch-test:ignore-err - test fixture setup
	if err := p.CancelPasswordReset("", testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("expected ErrInvalid on empty reason, got %v", err)
	}
}

// ----- Email change (token flow) --------------------------------------------

const validEmailChangeHash = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef" // 64 hex chars

func mustEmailChangeHash(t *testing.T, raw string) person.EmailChangeTokenHash {
	t.Helper()
	h, err := person.NewEmailChangeTokenHash(raw)
	if err != nil {
		t.Fatalf("NewEmailChangeTokenHash(%q): %v", raw, err)
	}
	return h
}

func TestRequestEmailChange_StoresPendingAndEmitsEvent(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	_ = p.PullEvents()

	newE := mustEmail(t, "new@b.io")
	h := mustEmailChangeHash(t, validEmailChangeHash)
	if err := p.RequestEmailChange(newE, "plaintext", h, time.Hour, testNow); err != nil {
		t.Fatalf("RequestEmailChange: %v", err)
	}
	pending := p.PendingEmailChange()
	if pending.IsZero() {
		t.Fatal("PendingEmailChange is zero")
	}
	if pending.NewEmail().String() != "new@b.io" {
		t.Errorf("NewEmail = %q", pending.NewEmail())
	}
	if !pending.Hash().Equal(h) {
		t.Error("hash mismatch")
	}
	expectedExpiry := testNow.Add(time.Hour)
	if !pending.ExpiresAt().Equal(expectedExpiry) {
		t.Errorf("ExpiresAt mismatch")
	}

	events := p.PullEvents()
	// Per ADR 0057: TWO events fire — audit + email-dispatch.
	if len(events) != 2 {
		t.Fatalf("events: %d, want 2", len(events))
	}
	ev, ok := events[0].(person.EmailChangeRequestedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want EmailChangeRequestedEvent", events[0])
	}
	if ev.NewEmail.String() != "new@b.io" {
		t.Errorf("event NewEmail = %q", ev.NewEmail)
	}
	confirmEv, ok := events[1].(person.EmailChangeConfirmationRequestedEvent)
	if !ok {
		t.Fatalf("event[1] = %T, want EmailChangeConfirmationRequestedEvent", events[1])
	}
	if confirmEv.PlaintextToken != "plaintext" {
		t.Errorf("dispatch plaintext = %q, want %q", confirmEv.PlaintextToken, "plaintext")
	}
	if confirmEv.NewEmail.String() != "new@b.io" {
		t.Errorf("dispatch NewEmail = %q", confirmEv.NewEmail)
	}
}

func TestRequestEmailChange_RejectsSameAddress(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustEmailChangeHash(t, validEmailChangeHash)
	if err := p.RequestEmailChange(p.Email(), "plaintext", h, time.Hour, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("same email: expected ErrInvalid, got %v", err)
	}
}

func TestRequestEmailChange_RejectsZeroAndInvalid(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustEmailChangeHash(t, validEmailChangeHash)

	if err := p.RequestEmailChange(email.Address{}, "plaintext", h, time.Hour, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("zero email: %v", err)
	}
	if err := p.RequestEmailChange(mustEmail(t, "new@b.io"), "plaintext", person.EmailChangeTokenHash{}, time.Hour, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("zero hash: %v", err)
	}
	if err := p.RequestEmailChange(mustEmail(t, "new@b.io"), "plaintext", h, 0, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("zero ttl: %v", err)
	}
}

func TestRequestEmailChange_NewSupersedesOld(t *testing.T) {
	t.Parallel()
	p := newPerson(t)

	first := mustEmailChangeHash(t, validEmailChangeHash)
	_ = p.RequestEmailChange(mustEmail(t, "first@b.io"), "plaintext-first", first, time.Hour, testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	second := mustEmailChangeHash(t, "00000000aaaaaaaa00000000aaaaaaaa00000000aaaaaaaa00000000aaaaaaaa")
	if err := p.RequestEmailChange(mustEmail(t, "second@b.io"), "plaintext-second", second, time.Hour, testNow); err != nil {
		t.Fatalf("second: %v", err)
	}
	pending := p.PendingEmailChange()
	if pending.NewEmail().String() != "second@b.io" {
		t.Errorf("NewEmail = %q, want second@b.io", pending.NewEmail())
	}
	if !pending.Hash().Equal(second) {
		t.Error("hash not superseded")
	}
}

func TestConfirmEmailChange_AppliesNewEmailAndRotatesStamp(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	originalStamp := p.SecurityStamp()
	originalEmail := p.Email()

	newE := mustEmail(t, "rotated@b.io")
	h := mustEmailChangeHash(t, validEmailChangeHash)
	_ = p.RequestEmailChange(newE, "plaintext", h, time.Hour, testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	if err := p.ConfirmEmailChange(h, testNow); err != nil {
		t.Fatalf("ConfirmEmailChange: %v", err)
	}
	if p.Email().String() != "rotated@b.io" {
		t.Errorf("Email not applied: %q", p.Email())
	}
	if p.SecurityStamp().Equal(originalStamp) {
		t.Error("SecurityStamp did not rotate")
	}
	if !p.PendingEmailChange().IsZero() {
		t.Error("pending not cleared")
	}

	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(person.EmailChangedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want EmailChangedEvent", events[0])
	}
	if ev.OldEmail.String() != originalEmail.String() {
		t.Errorf("OldEmail = %q, want %q", ev.OldEmail, originalEmail)
	}
	if ev.NewEmail.String() != "rotated@b.io" {
		t.Errorf("NewEmail = %q", ev.NewEmail)
	}
}

func TestConfirmEmailChange_RejectsExpired(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustEmailChangeHash(t, validEmailChangeHash)
	_ = p.RequestEmailChange(mustEmail(t, "new@b.io"), "plaintext", h, time.Hour, testNow) // arch-test:ignore-err - test fixture setup

	// Confirm AFTER the 1h TTL window — token expired.
	expired := testNow.Add(2 * time.Hour)
	if err := p.ConfirmEmailChange(h, expired); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("expired: expected ErrInvalid, got %v", err)
	}
	if !p.PendingEmailChange().IsZero() {
		t.Error("expired pending not defensively cleared")
	}
}

func TestConfirmEmailChange_RejectsMismatch(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	stored := mustEmailChangeHash(t, validEmailChangeHash)
	_ = p.RequestEmailChange(mustEmail(t, "new@b.io"), "plaintext", stored, time.Hour, testNow) // arch-test:ignore-err - test fixture setup

	wrong := mustEmailChangeHash(t, "fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe")
	if err := p.ConfirmEmailChange(wrong, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("mismatch: %v", err)
	}
	if p.PendingEmailChange().IsZero() {
		t.Error("pending should remain after mismatch")
	}
}

func TestConfirmEmailChange_RejectsWhenNoPending(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustEmailChangeHash(t, validEmailChangeHash)
	if err := p.ConfirmEmailChange(h, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("no pending: %v", err)
	}
}

func TestCancelEmailChange_ClearsPendingAndEmitsEvent(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	h := mustEmailChangeHash(t, validEmailChangeHash)
	_ = p.RequestEmailChange(mustEmail(t, "new@b.io"), "plaintext", h, time.Hour, testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	if err := p.CancelEmailChange("operator-cleared", testNow); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !p.PendingEmailChange().IsZero() {
		t.Error("pending not cleared")
	}
	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(person.EmailChangeCancelledEvent); !ok {
		t.Errorf("event[0] = %T, want EmailChangeCancelledEvent", events[0])
	}
}

func TestCancelEmailChange_NoOp_WhenNoPending(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()
	if err := p.CancelEmailChange("preempt", testNow); err != nil {
		t.Fatalf("Cancel no-op: %v", err)
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

// ----- GloballySuspend ------------------------------------------------------

func TestGloballySuspend_FlipsFlagAndRotatesStamp(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	_ = p.PullEvents()
	originalStamp := p.SecurityStamp()

	if err := p.GloballySuspend("compliance-violation-PCI-DSS", testNow); err != nil {
		t.Fatalf("GloballySuspend: %v", err)
	}
	if !p.IsGloballySuspended() {
		t.Error("IsGloballySuspended() = false after suspend")
	}
	if p.GlobalSuspensionReason() != "compliance-violation-PCI-DSS" {
		t.Errorf("Reason = %q", p.GlobalSuspensionReason())
	}
	if p.GloballySuspendedAt().IsZero() {
		t.Error("GloballySuspendedAt is zero")
	}
	if p.SecurityStamp().Equal(originalStamp) {
		t.Error("SecurityStamp did not rotate on global suspend")
	}

	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(person.GloballySuspendedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want GloballySuspendedEvent", events[0])
	}
	if ev.Reason != "compliance-violation-PCI-DSS" {
		t.Errorf("event Reason = %q", ev.Reason)
	}
}

func TestGloballySuspend_RequiresReason(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	for _, raw := range []string{"", "   ", "\t"} {
		if err := p.GloballySuspend(raw, testNow); !errors.Is(err, person.ErrInvalid) {
			t.Errorf("GloballySuspend(%q): expected ErrInvalid, got %v", raw, err)
		}
	}
}

func TestGloballySuspend_IdempotentOnSameReason(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.GloballySuspend("fraud", testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()
	if err := p.GloballySuspend("fraud", testNow); err != nil {
		t.Errorf("idempotent same reason: %v", err)
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on idempotent re-suspend, got %d", len(got))
	}
}

func TestGloballySuspend_RejectedOnDifferentReason(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.GloballySuspend("fraud", testNow) // arch-test:ignore-err - test fixture setup
	err := p.GloballySuspend("compliance", testNow)
	if !errors.Is(err, person.ErrInvalid) {
		t.Errorf("expected ErrInvalid on conflicting reason, got %v", err)
	}
}

func TestGloballySuspend_RejectedOnAnonymisedPerson(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.Anonymise(testNow) // arch-test:ignore-err - test fixture setup
	if err := p.GloballySuspend("fraud", testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("expected ErrInvalid on anonymised person, got %v", err)
	}
}

func TestLiftGlobalSuspension_ClearsFlagAndEmitsEvent(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.GloballySuspend("temporary-investigation", testNow) // arch-test:ignore-err - test fixture setup
	_ = p.PullEvents()

	if err := p.LiftGlobalSuspension(testNow); err != nil {
		t.Fatalf("LiftGlobalSuspension: %v", err)
	}
	if p.IsGloballySuspended() {
		t.Error("still IsGloballySuspended after lift")
	}
	if p.GlobalSuspensionReason() != "" {
		t.Errorf("reason not cleared: %q", p.GlobalSuspensionReason())
	}
	if !p.GloballySuspendedAt().IsZero() {
		t.Error("GloballySuspendedAt not cleared")
	}

	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(person.GlobalSuspensionLiftedEvent); !ok {
		t.Errorf("event[0] = %T, want GlobalSuspensionLiftedEvent", events[0])
	}
}

func TestLiftGlobalSuspension_NoOp_WhenNotSuspended(t *testing.T) {
	t.Parallel()
	p := newPerson(t)
	_ = p.PullEvents()
	if err := p.LiftGlobalSuspension(testNow); err != nil {
		t.Fatalf("Lift on not-suspended: %v", err)
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on no-op lift, got %d", len(got))
	}
}

// ----- Anonymise (DPDP/GDPR) ------------------------------------------------

func TestAnonymise_MarksAnonymisedAndScrubsFields(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	_ = p.PullEvents()

	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}
	if !p.IsAnonymised() {
		t.Error("IsAnonymised() = false after Anonymise()")
	}
	if p.IsActive() {
		t.Error("IsActive() should flip to false on anonymisation")
	}
	if p.FirstName() != "anonymised" || p.LastName() != "anonymised" {
		t.Errorf("name not scrubbed: first=%q last=%q", p.FirstName(), p.LastName())
	}
	if !p.AnonymisedAt().Equal(testNow) {
		t.Errorf("AnonymisedAt = %v", p.AnonymisedAt())
	}

	events := p.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(person.AnonymisedEvent); !ok {
		t.Errorf("event[0] = %T, want AnonymisedEvent", events[0])
	}
}

func TestAnonymise_Idempotent(t *testing.T) {
	t.Parallel()

	p := newPerson(t)
	_ = p.PullEvents()

	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("first Anonymise: %v", err)
	}
	_ = p.PullEvents()

	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("second Anonymise: %v", err)
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on idempotent anonymise, got %d", len(got))
	}
}

// ----- Re-hydration --------------------------------------------------------

func TestUnmarshalFromDB_DoesNotEmitEvents(t *testing.T) {
	t.Parallel()
	stamp := person.NewSecurityStamp()
	p := person.UnmarshalFromDB(person.Snapshot{
		ID:            person.ID(ids.NewV7().String()),
		Email:         mustEmail(t, "a@b.io"),
		FirstName:     "A",
		LastName:      "B",
		PasswordHash:  mustHash(t),
		SecurityStamp: stamp,
		IsActive:      true,
		IsAnonymised:  false,
		CreatedAt:     time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if p == nil {
		t.Fatal("UnmarshalFromDB returned nil")
	}
	if !p.SecurityStamp().Equal(stamp) {
		t.Error("SecurityStamp not preserved")
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("re-hydration emitted %d events, want 0", len(got))
	}
}

// ----- Helpers -------------------------------------------------------------

func newPerson(t *testing.T) *person.Person {
	t.Helper()
	p, err := person.New(newID(t), mustEmail(t, "a@b.io"), "A", "B", mustHash(t), testNow)
	if err != nil {
		t.Fatalf("newPerson: %v", err)
	}
	return p
}
