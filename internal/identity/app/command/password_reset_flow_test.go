package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// resettableRepo extends the existing fakePersonRepo behaviour with a
// hash → Person index so the confirm flow's GetByPasswordResetTokenHash
// lookup works against in-memory state.
type resettableRepo struct {
	*fakePersonRepo
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

func TestRequestPasswordReset_HappyPath_PersistsAndSendsEmail(t *testing.T) {
	t.Parallel()
	freezeClock(t)

	addr, err := email.New("alice@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	repo := newResettableRepo(newPersonWithPassword(t, "current-pw"))
	rec := email.NewRecorder(time.Now)

	from, _ := email.New("no-reply@leadkart.test")
	h := command.NewRequestPasswordResetHandler(repo, rec, from)

	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if repo.person.PendingPasswordReset().IsZero() {
		t.Error("expected pending password reset to be persisted")
	}
	if got := len(rec.Sent()); got != 1 {
		t.Errorf("recorder Sent() = %d, want 1", got)
	}
}

func TestRequestPasswordReset_UnknownEmail_SilentSuccess(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	repo := newResettableRepo(nil) // no Person seeded
	rec := email.NewRecorder(time.Now)
	from, _ := email.New("no-reply@leadkart.test")
	h := command.NewRequestPasswordResetHandler(repo, rec, from)

	addr, _ := email.New("unknown@example.test")
	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("expected silent success, got %v", err)
	}
	if got := len(rec.Sent()); got != 0 {
		t.Errorf("recorder Sent() = %d, want 0 (silent — no enumeration)", got)
	}
}

func TestConfirmPasswordReset_HappyPath_RotatesPasswordAndStamp(t *testing.T) {
	t.Parallel()
	freezeClock(t)

	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(p)
	rec := email.NewRecorder(time.Now)
	from, _ := email.New("no-reply@leadkart.test")

	reqHandler := command.NewRequestPasswordResetHandler(repo, rec, from)
	if err := reqHandler.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	// Recover the plaintext from the recorded email link.
	if len(rec.Sent()) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(rec.Sent()))
	}
	body := rec.Sent()[0].BodyText()
	rawToken := extractTokenFromLink(t, body)
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

// extractTokenFromLink scans the email body for the
// "?token=..." query value and returns it.
func extractTokenFromLink(t *testing.T, body string) string {
	t.Helper()
	const marker = "?token="
	i := indexOfMarker(body, marker)
	if i < 0 {
		t.Fatalf("token marker not found in body: %s", body)
	}
	rest := body[i+len(marker):]
	end := indexOfMarker(rest, "\n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func indexOfMarker(s, m string) int {
	for i := 0; i+len(m) <= len(s); i++ {
		if s[i:i+len(m)] == m {
			return i
		}
	}
	return -1
}
