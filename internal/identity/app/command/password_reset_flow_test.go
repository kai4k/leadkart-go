package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// resettableRepo extends the existing fakePersonRepo behaviour with a
// hash → Person index so the confirm flow's GetByPasswordResetTokenHash
// lookup works against in-memory state.
//
// drainedEvents captures the aggregate's pulled events after each
// successful UpdateByID closure — gives tests post-EDA visibility into
// the [person.PasswordResetEmailRequestedEvent] payload (carrying the
// plaintext token per ADR 0057) since the email gateway no longer
// records the body inline.
type resettableRepo struct {
	*fakePersonRepo
	drainedEvents []person.Event
}

func newResettableRepo(p *person.Person) *resettableRepo {
	return &resettableRepo{fakePersonRepo: &fakePersonRepo{person: p}}
}

func (r *resettableRepo) GetByEmail(_ context.Context, e email.Address) (*person.Person, error) {
	if r.person == nil || r.person.Email().String() != e.String() {
		return nil, person.ErrNotFound
	}
	return r.person, nil
}

func (r *resettableRepo) GetByPasswordResetTokenHash(_ context.Context, h person.PasswordResetTokenHash) (*person.Person, error) {
	if r.person == nil {
		return nil, person.ErrNotFound
	}
	pending := r.person.PendingPasswordReset()
	if pending.IsZero() || !pending.Hash().Equal(h) {
		return nil, person.ErrNotFound
	}
	return r.person, nil
}

// UpdateByID overrides the embedded fake's variant to additionally
// drain events on commit — mirrors the pg adapter's drainPersonEvents
// path so the test sees the same shape production sees post-Wave-9.2d.
func (r *resettableRepo) UpdateByID(_ context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	if r.person == nil || r.person.ID() != id {
		return person.ErrNotFound
	}
	commit, err := fn(r.person)
	if err != nil {
		return err
	}
	if commit {
		r.drainedEvents = append(r.drainedEvents, r.person.PullEvents()...)
	}
	return nil
}

// emailRequestedToken extracts the plaintext from the most recent
// PasswordResetEmailRequestedEvent captured by drainedEvents.
func (r *resettableRepo) emailRequestedToken(t *testing.T) string {
	t.Helper()
	for _, e := range r.drainedEvents {
		if ev, ok := e.(person.PasswordResetEmailRequestedEvent); ok {
			return ev.PlaintextToken
		}
	}
	t.Fatal("no PasswordResetEmailRequestedEvent captured")
	return ""
}

func TestRequestPasswordReset_HappyPath_PersistsAndEmitsEmailEvent(t *testing.T) {
	t.Parallel()
	freezeClock(t)

	addr, err := email.New("alice@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	repo := newResettableRepo(newPersonWithPassword(t, "current-pw"))

	h := command.NewRequestPasswordResetHandler(repo)

	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if repo.person.PendingPasswordReset().IsZero() {
		t.Error("expected pending password reset to be persisted")
	}
	// Per ADR 0057: the email is delivered async via a Watermill
	// subscriber. The handler's contract is "emit the dispatch event";
	// the subscriber-side test in ports/subscribers covers the actual
	// gateway.Send. Assert the event was recorded.
	if tok := repo.emailRequestedToken(t); tok == "" {
		t.Error("expected non-empty plaintext token on dispatch event")
	}
}

func TestRequestPasswordReset_UnknownEmail_SilentSuccess(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	repo := newResettableRepo(nil) // no Person seeded
	h := command.NewRequestPasswordResetHandler(repo)

	addr, _ := email.New("unknown@example.test")
	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("expected silent success, got %v", err)
	}
	if got := len(repo.drainedEvents); got != 0 {
		t.Errorf("drained events = %d, want 0 (silent — no enumeration, no dispatch event)", got)
	}
}

func TestConfirmPasswordReset_HappyPath_RotatesPasswordAndStamp(t *testing.T) {
	t.Parallel()
	freezeClock(t)

	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(p)

	reqHandler := command.NewRequestPasswordResetHandler(repo)
	if err := reqHandler.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	// Recover the plaintext from the captured dispatch event (the
	// async subscriber would receive the same payload).
	rawToken := repo.emailRequestedToken(t)
	stampBefore := p.SecurityStamp()

	confirmHandler := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{})
	if err := confirmHandler.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: "Tr0ub4dor&3-newly-strong",
	}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !p.PendingPasswordReset().IsZero() {
		t.Error("expected pending reset cleared after confirm")
	}
	if p.SecurityStamp().String() == stampBefore.String() {
		t.Error("expected SecurityStamp rotated")
	}
}

func TestConfirmPasswordReset_BadToken_ReturnsTokenInvalid(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	repo := newResettableRepo(newPersonWithPassword(t, "current-pw"))
	h := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{})
	err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    "totally-bogus-token",
		NewPassword: "anything",
	})
	if !errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatalf("err = %v, want ErrResetTokenInvalid", err)
	}
}

