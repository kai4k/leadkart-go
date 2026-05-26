package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
)

// errBoom is a marker used by the failing-repo + failing-breach-checker
// doubles below so tests can assert error propagation without coupling
// to a specific message string.
var errBoom = errors.New("boom")

// failingPersonsRepo embeds the shared fake but overrides specific
// method(s) to return errBoom for the targeted lookup/persist branch.
// Construct with helper constructors below; keep the override surface
// minimal so the rest of the contract still flows through the fake.
type failingPersonsRepo struct {
	*persontest.FakeRepository
	failGetByEmail    bool
	failGetByTokenHash bool
	failUpdateByID    bool
	failUpdateErr     error // override the err returned (nil → errBoom)
}

func (f *failingPersonsRepo) GetByEmail(ctx context.Context, e email.Address) (*person.Person, error) {
	if f.failGetByEmail {
		return nil, errBoom
	}
	return f.FakeRepository.GetByEmail(ctx, e)
}

func (f *failingPersonsRepo) GetByPasswordResetTokenHash(ctx context.Context, hash person.PasswordResetTokenHash) (*person.Person, error) {
	if f.failGetByTokenHash {
		return nil, errBoom
	}
	return f.FakeRepository.GetByPasswordResetTokenHash(ctx, hash)
}

func (f *failingPersonsRepo) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	if f.failUpdateByID {
		err := f.failUpdateErr
		if err == nil {
			err = errBoom
		}
		return err
	}
	return f.FakeRepository.UpdateByID(ctx, id, fn)
}

// breachingChecker is a [passwordpolicy.Checker] that reports every
// password as breached. Used to exercise the breach-rejection branch
// without coupling to the OfflineList contents.
type breachingChecker struct{}

func (breachingChecker) IsBreached(_ context.Context, _ string) (bool, error) {
	return true, nil
}

// failingBreachChecker returns an error from IsBreached — exercises the
// "lookup failure" branch where the handler MUST surface the wrap, not
// fail-open.
type failingBreachChecker struct{}

func (failingBreachChecker) IsBreached(_ context.Context, _ string) (bool, error) {
	return false, errBoom
}


// resettableRepo wraps the shared [persontest.FakeRepository] with the
// drained-events capture the password-reset flow assertions need. The
// shared fake matches the SQL adapter's contract semantics
// (GetByPasswordResetTokenHash hashes match against the pending
// reset sub-state); this wrapper adds drainPersonEvents-equivalent
// observability so tests can assert on the
// [person.PasswordResetEmailRequestedEvent] payload (carrying the
// plaintext token per ADR 0057) — the email gateway no longer records
// the body inline.
type resettableRepo struct {
	*persontest.FakeRepository
	seeded        *person.Person // the one Person under test, retained for event drain
	drainedEvents []person.Event
}

func newResettableRepo(t *testing.T, p *person.Person) *resettableRepo {
	t.Helper()
	inner := persontest.NewFakeRepository()
	if p != nil {
		if err := inner.Add(t.Context(), p); err != nil {
			t.Fatalf("newResettableRepo: Add: %v", err)
		}
	}
	return &resettableRepo{FakeRepository: inner, seeded: p}
}

// UpdateByID overrides the embedded fake's variant to additionally
// drain events on commit — mirrors the pg adapter's drainPersonEvents
// path so the test sees the same shape production sees post-Wave-9.2d.
func (r *resettableRepo) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	if err := r.FakeRepository.UpdateByID(ctx, id, fn); err != nil {
		return err
	}
	if r.seeded != nil && r.seeded.ID() == id {
		r.drainedEvents = append(r.drainedEvents, r.seeded.PullEvents()...)
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
	// Safe to parallelise post-clock-injection: each handler carries its
	// own `now func() time.Time` closure so different tests can use
	// different instants concurrently without racing on a global.
	t.Parallel()

	addr, err := email.New("alice@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)

	h := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })

	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if p.PendingPasswordReset().IsZero() {
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
	repo := newResettableRepo(t, nil) // no Person seeded
	h := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })

	addr, _ := email.New("unknown@example.test")
	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("expected silent success, got %v", err)
	}
	if got := len(repo.drainedEvents); got != 0 {
		t.Errorf("drained events = %d, want 0 (silent — no enumeration, no dispatch event)", got)
	}
}

func TestConfirmPasswordReset_HappyPath_RotatesPasswordAndStamp(t *testing.T) {
	// Safe to parallelise post-clock-injection — this test was the
	// cloud-CI flake's primary surface (a parallel permission_request
	// test's freezeClock to 2026-05-23 overwrote this test's 2026-05-07,
	// pushing the just-minted reset token past its 1h expiry window).
	// With explicit-time injection, each test threads its own instant
	// through the handler; cross-test interference is structurally
	// impossible.
	t.Parallel()

	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)

	reqHandler := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })
	if err := reqHandler.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	// Recover the plaintext from the captured dispatch event (the
	// async subscriber would receive the same payload).
	rawToken := repo.emailRequestedToken(t)
	stampBefore := p.SecurityStamp()

	confirmHandler := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
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
	repo := newResettableRepo(t, newPersonWithPassword(t, "current-pw"))
	h := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    "totally-bogus-token",
		NewPassword: "anything",
	})
	if !errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatalf("err = %v, want ErrResetTokenInvalid", err)
	}
}

// ----- ConfirmPasswordReset — input + lookup branch coverage --------------

// TestConfirmPasswordReset_InputRejections is a table-driven cover of the
// boundary-input + format-rejection arms. Per the enumeration-safety
// rule (security.md "Password reset"): EVERY failure surfaces a single
// sentinel — ErrResetTokenInvalid or ErrNewPasswordRequired — never a
// driver-leak.
func TestConfirmPasswordReset_InputRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  command.ConfirmPasswordResetCommand
		want error
	}{
		{
			name: "empty raw token",
			cmd:  command.ConfirmPasswordResetCommand{RawToken: "", NewPassword: "x"},
			want: command.ErrResetTokenInvalid,
		},
		{
			name: "empty new password",
			cmd:  command.ConfirmPasswordResetCommand{RawToken: "anything", NewPassword: ""},
			want: command.ErrNewPasswordRequired,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := persontest.NewFakeRepository()
			h := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
			err := h.Handle(t.Context(), c.cmd)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestConfirmPasswordReset_LookupError_Wrapped exercises the generic
// (non-NotFound) lookup-error branch. The handler MUST wrap with
// "confirm_password_reset: lookup: %w" — i.e. propagate the cause via
// errors.Is, NOT collapse to ErrResetTokenInvalid. Collapsing would
// hide infrastructure failures behind a benign-looking 422.
func TestConfirmPasswordReset_LookupError_Wrapped(t *testing.T) {
	t.Parallel()
	repo := &failingPersonsRepo{
		FakeRepository:     persontest.NewFakeRepository(),
		failGetByTokenHash: true,
	}
	h := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    "non-empty-token-bytes",
		NewPassword: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
	if errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatal("lookup error must NOT collapse to ErrResetTokenInvalid")
	}
}

// TestConfirmPasswordReset_AnonymisedPerson_ReturnsTokenInvalid mirrors
// the request-side suppression: anonymised Persons cannot reset; surface
// as token-invalid (no account-state disclosure).
func TestConfirmPasswordReset_AnonymisedPerson_ReturnsTokenInvalid(t *testing.T) {
	t.Parallel()
	// Seed a Person + mint a real reset token via the request flow, then
	// anonymise the Person before confirm.
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	rawToken := repo.emailRequestedToken(t)
	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}

	confirm := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
	err := confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatalf("err = %v, want ErrResetTokenInvalid", err)
	}
}

// TestConfirmPasswordReset_GloballySuspendedPerson_ReturnsTokenInvalid
// mirrors the anonymised arm — globally-suspended Persons cannot reset.
func TestConfirmPasswordReset_GloballySuspendedPerson_ReturnsTokenInvalid(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	rawToken := repo.emailRequestedToken(t)
	if err := p.GloballySuspend("compliance: ban", testNow); err != nil {
		t.Fatalf("GloballySuspend: %v", err)
	}

	confirm := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
	err := confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatalf("err = %v, want ErrResetTokenInvalid", err)
	}
}

// TestConfirmPasswordReset_SameAsCurrent_Rejected verifies the handler-
// layer no-op suppression. The aggregate doesn't enforce this; the
// handler does so the audit log doesn't fill with "reset to same
// password" noise.
func TestConfirmPasswordReset_SameAsCurrent_Rejected(t *testing.T) {
	t.Parallel()
	const samePw = "current-pw-and-also-new"
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, samePw)
	repo := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	rawToken := repo.emailRequestedToken(t)

	confirm := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
	err := confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: samePw,
	})
	if !errors.Is(err, command.ErrPasswordSameAsCurrent) {
		t.Fatalf("err = %v, want ErrPasswordSameAsCurrent", err)
	}
}

// TestConfirmPasswordReset_BreachCheckerError_Wrapped verifies the
// handler does NOT fail-open on a breach-checker lookup failure
// (network / ratelimit / provider error). Per security.md: the right
// shape is a wrapped error that lets the HTTP layer surface 503.
func TestConfirmPasswordReset_BreachCheckerError_Wrapped(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	rawToken := repo.emailRequestedToken(t)

	confirm := command.NewConfirmPasswordResetHandler(repo, failingBreachChecker{}, func() time.Time { return testNow })
	err := confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
}

// TestConfirmPasswordReset_Breached_Rejected verifies the explicit
// breach-rejection arm with a checker that reports every password as
// breached. ErrPasswordBreached → HTTP 422.
func TestConfirmPasswordReset_Breached_Rejected(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	repo := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	rawToken := repo.emailRequestedToken(t)

	confirm := command.NewConfirmPasswordResetHandler(repo, breachingChecker{}, func() time.Time { return testNow })
	err := confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: "any-password-the-checker-will-flag",
	})
	if !errors.Is(err, command.ErrPasswordBreached) {
		t.Fatalf("err = %v, want ErrPasswordBreached", err)
	}
}

// TestConfirmPasswordReset_PersistInvalid_CollapsesToTokenInvalid
// exercises the aggregate-rejection arm: the closure's UpdateByID
// returns person.ErrInvalid (e.g. token mismatch / expired) — the
// handler collapses to ErrResetTokenInvalid per the enumeration-safety
// rule (same wire shape regardless of cause).
func TestConfirmPasswordReset_PersistInvalid_CollapsesToTokenInvalid(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	inner := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(inner, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	rawToken := inner.emailRequestedToken(t)

	repo := &failingPersonsRepo{
		FakeRepository: inner.FakeRepository,
		failUpdateByID: true,
		failUpdateErr:  person.ErrInvalid,
	}
	confirm := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
	err := confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatalf("err = %v, want ErrResetTokenInvalid (collapsed from person.ErrInvalid)", err)
	}
}

// TestConfirmPasswordReset_PersistError_Wrapped exercises the generic-
// (non-Invalid) persist-error branch — surfaces wrapped, NOT collapsed.
func TestConfirmPasswordReset_PersistError_Wrapped(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	inner := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(inner, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Request: %v", err)
	}
	rawToken := inner.emailRequestedToken(t)

	repo := &failingPersonsRepo{
		FakeRepository: inner.FakeRepository,
		failUpdateByID: true,
		failUpdateErr:  errBoom,
	}
	confirm := command.NewConfirmPasswordResetHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })
	err := confirm.Handle(t.Context(), command.ConfirmPasswordResetCommand{
		RawToken:    rawToken,
		NewPassword: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
	if errors.Is(err, command.ErrResetTokenInvalid) {
		t.Fatal("generic persist error must NOT collapse to ErrResetTokenInvalid")
	}
}

// ----- RequestPasswordReset — input + lookup branch coverage --------------

// TestRequestPasswordReset_RejectsZeroEmail mirrors the boundary check
// before any repo dependency is touched.
func TestRequestPasswordReset_RejectsZeroEmail(t *testing.T) {
	t.Parallel()
	repo := persontest.NewFakeRepository()
	h := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: email.Address{}})
	if err == nil {
		t.Fatal("expected error for zero email, got nil")
	}
}

// TestRequestPasswordReset_LookupError_Wrapped exercises the (non-
// NotFound) lookup-error arm. Wrapped, NOT silent — operators see the
// alert; the silent-success branch is reserved for the
// enumeration-safe known cases (NotFound / anonymised / suspended).
func TestRequestPasswordReset_LookupError_Wrapped(t *testing.T) {
	t.Parallel()
	repo := &failingPersonsRepo{
		FakeRepository: persontest.NewFakeRepository(),
		failGetByEmail: true,
	}
	addr, _ := email.New("alice@example.test")
	h := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
}

// TestRequestPasswordReset_AnonymisedPerson_SilentSuccess proves the
// suppression branch — no token minted, no event dispatched.
func TestRequestPasswordReset_AnonymisedPerson_SilentSuccess(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}
	repo := newResettableRepo(t, p)

	h := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(repo.drainedEvents); got != 0 {
		t.Errorf("drained events = %d, want 0 (silent — anonymised)", got)
	}
	if !p.PendingPasswordReset().IsZero() {
		t.Error("expected NO pending reset on anonymised Person")
	}
}

// TestRequestPasswordReset_GloballySuspendedPerson_SilentSuccess
// mirrors the anonymised arm — globally-suspended Persons are
// silently suppressed (no suspension-state disclosure).
func TestRequestPasswordReset_GloballySuspendedPerson_SilentSuccess(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	if err := p.GloballySuspend("compliance: ban", testNow); err != nil {
		t.Fatalf("GloballySuspend: %v", err)
	}
	repo := newResettableRepo(t, p)

	h := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(repo.drainedEvents); got != 0 {
		t.Errorf("drained events = %d, want 0 (silent — globally suspended)", got)
	}
	if !p.PendingPasswordReset().IsZero() {
		t.Error("expected NO pending reset on globally-suspended Person")
	}
}

// TestRequestPasswordReset_PersistError_Wrapped exercises the
// UpdateByID-error arm. Wrapped — operator-visible.
func TestRequestPasswordReset_PersistError_Wrapped(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, "current-pw")
	// Wrap the underlying fake so GetByEmail returns the seeded Person
	// (handler proceeds past the silent-suppression arms), but
	// UpdateByID fails.
	inner := persontest.NewFakeRepository()
	if err := inner.Add(t.Context(), p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repo := &failingPersonsRepo{
		FakeRepository: inner,
		failUpdateByID: true,
		failUpdateErr:  errBoom,
	}
	h := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
}

// TestNewRequestPasswordResetHandler_PanicsOnNilRepo locks the wiring
// contract.
func TestNewRequestPasswordResetHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil persons repo")
		}
	}()
	_ = command.NewRequestPasswordResetHandler(nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
}

// TestNewConfirmPasswordResetHandler_PanicsOnNilDeps locks the wiring
// contract for both required deps.
func TestNewConfirmPasswordResetHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	t.Run("nil persons", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil persons repo")
			}
		}()
		_ = command.NewConfirmPasswordResetHandler(nil, passwordpolicy.Noop{}, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
	t.Run("nil breach checker", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil breach checker")
			}
		}()
		_ = command.NewConfirmPasswordResetHandler(persontest.NewFakeRepository(), nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
}

