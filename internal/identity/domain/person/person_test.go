package person_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

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
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	id := newID(t)
	e := mustEmail(t, "alice@example.com")
	p, err := person.New(id, e, "Alice", "Sharma", mustHash(t))
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
	if !p.CreatedAt().Equal(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v", p.CreatedAt())
	}
}

func TestNewPerson_EmitsCreatedEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	id := newID(t)
	p, err := person.New(id, mustEmail(t, "a@b.io"), "A", "B", mustHash(t))
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
			_, err := person.New(tc.id, tc.email, tc.firstName, tc.lastName, tc.hash)
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
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	p := newPerson(t)
	_ = p.PullEvents()
	originalStamp := p.SecurityStamp()

	newHash, _ := person.NewPasswordHash(
		"$argon2id$v=19$m=19456,t=2,p=1$bmV3c2FsdG5ld3NhbHQ$bmV3aGFzaG5ld2hhc2huZXc",
	)
	if err := p.ChangePassword(newHash); err != nil {
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
	t.Cleanup(clock.Reset)
	p := newPerson(t)
	if err := p.ChangePassword(person.PasswordHash{}); err == nil {
		t.Fatal("expected error on zero hash")
	}
}

// ----- UpdateProfile --------------------------------------------------------

func TestUpdateProfile_ChangesNameAndEmitsEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	p := newPerson(t)
	_ = p.PullEvents() // drain CreatedEvent

	if err := p.UpdateProfile("Alice", "Sharma-Khan"); err != nil {
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
	t.Cleanup(clock.Reset)
	p := newPerson(t)
	_ = p.PullEvents()

	if err := p.UpdateProfile("A", "B"); err != nil {
		t.Fatalf("UpdateProfile no-op: %v", err)
	}
	if got := p.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on no-op update, got %d", len(got))
	}
}

func TestUpdateProfile_RejectsEmptyAndOverlong(t *testing.T) {
	t.Cleanup(clock.Reset)
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
			p := newPerson(t)
			err := p.UpdateProfile(tc.firstName, tc.lastName)
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
	t.Cleanup(clock.Reset)
	p := newPerson(t)
	_ = p.Anonymise()
	_ = p.PullEvents()

	err := p.UpdateProfile("Alice", "Sharma")
	if err == nil {
		t.Fatal("expected error updating anonymised person")
	}
	if !errors.Is(err, person.ErrInvalid) {
		t.Errorf("expected errors.Is(_, ErrInvalid), got %v", err)
	}
}

// ----- Anonymise (DPDP/GDPR) ------------------------------------------------

func TestAnonymise_MarksAnonymisedAndScrubsFields(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	p := newPerson(t)
	_ = p.PullEvents()

	clock.Set(time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC))
	if err := p.Anonymise(); err != nil {
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
	if !p.AnonymisedAt().Equal(time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)) {
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
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	p := newPerson(t)
	_ = p.PullEvents()

	if err := p.Anonymise(); err != nil {
		t.Fatalf("first Anonymise: %v", err)
	}
	_ = p.PullEvents()

	if err := p.Anonymise(); err != nil {
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
	p, err := person.New(newID(t), mustEmail(t, "a@b.io"), "A", "B", mustHash(t))
	if err != nil {
		t.Fatalf("newPerson: %v", err)
	}
	return p
}
