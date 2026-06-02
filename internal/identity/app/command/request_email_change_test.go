package command_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
)

// TestNewRequestEmailChangeHandler_PanicsOnNilDeps — persons repo is required.
// Per ADR 0057 there is no email gateway dep (delivery is via outbox).
func TestNewRequestEmailChangeHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil persons repo")
		}
	}()
	_ = command.NewRequestEmailChangeHandler(nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
}

// TestRequestEmailChange_RejectsZeroPersonID — input guard before any repo call.
func TestRequestEmailChange_RejectsZeroPersonID(t *testing.T) {
	t.Parallel()
	addr, err := email.New("new@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	repo := seedPersonRepo(t, nil)
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err = h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: person.ID(""),
		NewEmail: addr,
	})
	if err == nil {
		t.Fatal("expected error for zero person id, got nil")
	}
}

// TestRequestEmailChange_RejectsZeroEmail — guard on the email VO.
func TestRequestEmailChange_RejectsZeroEmail(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	repo := seedPersonRepo(t, p)
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: p.ID(),
		NewEmail: email.Address{},
	})
	if err == nil {
		t.Fatal("expected error for zero email, got nil")
	}
}

// TestRequestEmailChange_PersonNotFound_ReturnsErrEmailChangeRejected — a
// missing Person collapses to the generic rejection (enumeration-safety).
func TestRequestEmailChange_PersonNotFound_ReturnsErrEmailChangeRejected(t *testing.T) {
	t.Parallel()
	addr, err := email.New("new@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	repo := seedPersonRepo(t, nil) // no Person seeded
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err = h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: person.ID("p-missing-1"),
		NewEmail: addr,
	})
	if !errors.Is(err, command.ErrEmailChangeRejected) {
		t.Fatalf("err = %v, want ErrEmailChangeRejected", err)
	}
}

// TestRequestEmailChange_SameAsCurrent_Rejected — requesting the current
// address collapses to the generic rejection before token mint.
func TestRequestEmailChange_SameAsCurrent_Rejected(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	repo := seedPersonRepo(t, p)
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: p.ID(),
		NewEmail: p.Email(),
	})
	if !errors.Is(err, command.ErrEmailChangeRejected) {
		t.Fatalf("err = %v, want ErrEmailChangeRejected", err)
	}
}

// Additional branch coverage for RequestEmailChange + ConfirmEmailChangeHandler
// (ADR 0062 §6). b2EmailPersonRepo injects errors on the four call sites
// (GetByID / GetByEmail / GetByEmailChangeTokenHash / UpdateByID).
type b2EmailPersonRepo struct {
	*persontest.FakeRepository

	errOnGetByID                   error
	errOnGetByEmail                error
	errOnGetByEmailChangeTokenHash error
	errOnUpdateByID                error
}

func newB2EmailPersonRepo(t *testing.T, p *person.Person) *b2EmailPersonRepo {
	t.Helper()
	inner := persontest.NewFakeRepository()
	if p != nil {
		if err := inner.Add(t.Context(), p); err != nil {
			t.Fatalf("newB2EmailPersonRepo: Add: %v", err)
		}
	}
	return &b2EmailPersonRepo{FakeRepository: inner}
}

func (r *b2EmailPersonRepo) GetByID(ctx context.Context, id person.ID) (*person.Person, error) {
	if r.errOnGetByID != nil {
		err := r.errOnGetByID
		r.errOnGetByID = nil
		return nil, err
	}
	return r.FakeRepository.GetByID(ctx, id)
}

func (r *b2EmailPersonRepo) GetByEmail(ctx context.Context, e email.Address) (*person.Person, error) {
	if r.errOnGetByEmail != nil {
		err := r.errOnGetByEmail
		r.errOnGetByEmail = nil
		return nil, err
	}
	return r.FakeRepository.GetByEmail(ctx, e)
}

func (r *b2EmailPersonRepo) GetByEmailChangeTokenHash(ctx context.Context, h person.EmailChangeTokenHash) (*person.Person, error) {
	if r.errOnGetByEmailChangeTokenHash != nil {
		err := r.errOnGetByEmailChangeTokenHash
		r.errOnGetByEmailChangeTokenHash = nil
		return nil, err
	}
	return r.FakeRepository.GetByEmailChangeTokenHash(ctx, h)
}

func (r *b2EmailPersonRepo) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	if r.errOnUpdateByID != nil {
		err := r.errOnUpdateByID
		r.errOnUpdateByID = nil
		return err
	}
	return r.FakeRepository.UpdateByID(ctx, id, fn)
}

var errB2EmailChange = errors.New("b2: synthetic infrastructure failure (email-change)")

// b2NewSecondaryPerson builds a second Person (different ID + email) for the
// collision tests.
func b2NewSecondaryPerson(t *testing.T, addr email.Address) *person.Person {
	t.Helper()
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=1$c29tZXNhbHQAAAAA$WjQXjLDXrEPYz8KGRwl9N6c1L+sM5n5L0c0kMmH3vLU")
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	p, err := person.New(
		person.ID("p-secondary-collision-1"), addr, "Bob", "Other", hash, testNow,
	)
	if err != nil {
		t.Fatalf("person.New (secondary): %v", err)
	}
	return p
}

// ----- RequestEmailChange — additional branches ----------------------------

func TestRequestEmailChange_GetByID_NonNotFoundError_Wrapped(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("new@example.test")
	repo := newB2EmailPersonRepo(t, nil)
	repo.errOnGetByID = errB2EmailChange

	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: person.ID("p-anything"),
		NewEmail: addr,
	})
	if !errors.Is(err, errB2EmailChange) {
		t.Fatalf("err = %v, want wrapped errB2EmailChange", err)
	}
	if errors.Is(err, command.ErrEmailChangeRejected) {
		t.Errorf("non-NotFound infra error MUST NOT collapse to ErrEmailChangeRejected")
	}
}

func TestRequestEmailChange_AnonymisedPerson_Rejected(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}
	repo := seedPersonRepo(t, p)
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	addr, _ := email.New("new@example.test")
	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: p.ID(),
		NewEmail: addr,
	})
	if !errors.Is(err, command.ErrEmailChangeRejected) {
		t.Fatalf("err = %v, want ErrEmailChangeRejected", err)
	}
}

func TestRequestEmailChange_GloballySuspendedPerson_Rejected(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	if err := p.GloballySuspend("cross-tenant abuse", testNow); err != nil {
		t.Fatalf("GloballySuspend: %v", err)
	}
	repo := seedPersonRepo(t, p)
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	addr, _ := email.New("new@example.test")
	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: p.ID(),
		NewEmail: addr,
	})
	if !errors.Is(err, command.ErrEmailChangeRejected) {
		t.Fatalf("err = %v, want ErrEmailChangeRejected", err)
	}
}

func TestRequestEmailChange_NewEmailOwnedByDifferentPerson_AlreadyTaken(t *testing.T) {
	// Another Person owns the proposed email → ErrEmailAlreadyTaken (409).
	t.Parallel()

	p := newPersonWithPassword(t, "irrelevant")
	otherAddr, _ := email.New("taken@example.test")
	other := b2NewSecondaryPerson(t, otherAddr)
	repo := seedPersonRepo(t, p)
	if err := repo.Add(t.Context(), other); err != nil {
		t.Fatalf("seed other: %v", err)
	}
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: p.ID(),
		NewEmail: otherAddr,
	})
	if !errors.Is(err, command.ErrEmailAlreadyTaken) {
		t.Fatalf("err = %v, want ErrEmailAlreadyTaken", err)
	}
}

func TestRequestEmailChange_GetByEmail_NonNotFoundError_Wrapped(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	repo := newB2EmailPersonRepo(t, p)
	repo.errOnGetByEmail = errB2EmailChange

	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })
	addr, _ := email.New("new@example.test")
	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: p.ID(),
		NewEmail: addr,
	})
	if !errors.Is(err, errB2EmailChange) {
		t.Fatalf("err = %v, want wrapped errB2EmailChange", err)
	}
	if errors.Is(err, command.ErrEmailAlreadyTaken) {
		t.Errorf("non-NotFound collision-check error MUST NOT collapse to ErrEmailAlreadyTaken")
	}
}

func TestRequestEmailChange_UpdateByID_Error_Wrapped(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "irrelevant")
	repo := newB2EmailPersonRepo(t, p)
	repo.errOnUpdateByID = errB2EmailChange

	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })
	addr, _ := email.New("new@example.test")
	err := h.Handle(t.Context(), command.RequestEmailChangeCommand{
		PersonID: p.ID(),
		NewEmail: addr,
	})
	if !errors.Is(err, errB2EmailChange) {
		t.Fatalf("err = %v, want wrapped errB2EmailChange", err)
	}
}

// ----- ConfirmEmailChange — full coverage (no existing tests) ------------

// b2SeedPendingEmailChange applies a fresh confirmation token to p via
// RequestEmailChange and returns the plaintext. The hash matches
// hashEmailChangeToken (SHA-256 hex) so the handler's verification resolves.
func b2SeedPendingEmailChange(t *testing.T, p *person.Person, newAddr email.Address) string {
	t.Helper()
	plaintext := "deterministic-test-token-base64url-stable-length-32+chars"
	sum := sha256.Sum256([]byte(plaintext))
	hashHex := hex.EncodeToString(sum[:])
	hash, err := person.NewEmailChangeTokenHash(hashHex)
	if err != nil {
		t.Fatalf("NewEmailChangeTokenHash: %v", err)
	}
	if err := p.RequestEmailChange(newAddr, plaintext, hash, time.Hour, testNow); err != nil {
		t.Fatalf("RequestEmailChange: %v", err)
	}
	return plaintext
}

func TestNewConfirmEmailChangeHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil persons repo")
		}
	}()
	_ = command.NewConfirmEmailChangeHandler(nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
}

func TestConfirmEmailChange_HappyPath(t *testing.T) {
	t.Parallel()

	newAddr, _ := email.New("rotated@example.test")
	p := newPersonWithPassword(t, "irrelevant")
	rawToken := b2SeedPendingEmailChange(t, p, newAddr)
	repo := seedPersonRepo(t, p)
	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })

	if err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{
		RawToken: rawToken,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := repo.GetByID(t.Context(), p.ID())
	if err != nil {
		t.Fatalf("GetByID after confirm: %v", err)
	}
	if got.Email().String() != newAddr.String() {
		t.Errorf("Email after confirm = %q, want %q", got.Email().String(), newAddr.String())
	}
	if !got.PendingEmailChange().IsZero() {
		t.Error("PendingEmailChange should be cleared after confirm")
	}
}

func TestConfirmEmailChange_EmptyRawToken_TokenInvalid(t *testing.T) {
	t.Parallel()
	repo := seedPersonRepo(t, nil)
	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{RawToken: ""})
	if !errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Fatalf("err = %v, want ErrEmailChangeTokenInvalid", err)
	}
}

func TestConfirmEmailChange_HashWrapperFormatError_TokenInvalid(t *testing.T) {
	// The NewEmailChangeTokenHash format-error branch is unreachable for
	// SHA-256 output (always 64 hex chars), so this exercises the
	// runtime-reachable path: a non-existent hash lookup → token-invalid.
	t.Parallel()
	repo := seedPersonRepo(t, nil) // no Person seeded → GetByEmailChangeTokenHash returns ErrNotFound
	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{RawToken: "any-non-empty-raw-token"})
	if !errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Fatalf("err = %v, want ErrEmailChangeTokenInvalid", err)
	}
}

func TestConfirmEmailChange_LookupNotFound_TokenInvalid(t *testing.T) {
	// No Person in repo — GetByEmailChangeTokenHash returns ErrNotFound.
	t.Parallel()
	repo := seedPersonRepo(t, nil)
	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{
		RawToken: "raw-token-that-doesnt-match-any-seeded-pending",
	})
	if !errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Fatalf("err = %v, want ErrEmailChangeTokenInvalid", err)
	}
}

func TestConfirmEmailChange_Lookup_NonNotFoundError_Wrapped(t *testing.T) {
	t.Parallel()
	repo := newB2EmailPersonRepo(t, nil)
	repo.errOnGetByEmailChangeTokenHash = errB2EmailChange
	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{RawToken: "any-raw-token"})
	if !errors.Is(err, errB2EmailChange) {
		t.Fatalf("err = %v, want wrapped errB2EmailChange", err)
	}
	if errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Errorf("non-NotFound infra error MUST NOT collapse to ErrEmailChangeTokenInvalid")
	}
}

func TestConfirmEmailChange_AnonymisedPerson_TokenInvalid(t *testing.T) {
	t.Parallel()

	newAddr, _ := email.New("rotated@example.test")
	p := newPersonWithPassword(t, "irrelevant")
	_ = b2SeedPendingEmailChange(t, p, newAddr) // arch-test:ignore-err - seed-only; plaintext not needed
	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}
	// The fake still returns the row (no state filter) — the handler must
	// reject before UpdateByID.
	repo := seedPersonRepo(t, p)
	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{
		RawToken: "deterministic-test-token-base64url-stable-length-32+chars",
	})
	if !errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Fatalf("err = %v, want ErrEmailChangeTokenInvalid", err)
	}
}

func TestConfirmEmailChange_UpdateByID_PersonInvalidError_TokenInvalid(t *testing.T) {
	// person.ErrInvalid from UpdateByID translates to ErrEmailChangeTokenInvalid.
	t.Parallel()

	newAddr, _ := email.New("rotated@example.test")
	p := newPersonWithPassword(t, "irrelevant")
	_ = b2SeedPendingEmailChange(t, p, newAddr) // arch-test:ignore-err - seed-only; plaintext not needed
	repo := newB2EmailPersonRepo(t, p)
	repo.errOnUpdateByID = person.ErrInvalid

	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{
		RawToken: "deterministic-test-token-base64url-stable-length-32+chars",
	})
	if !errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Fatalf("err = %v, want ErrEmailChangeTokenInvalid (person.ErrInvalid translation)", err)
	}
}

func TestConfirmEmailChange_UpdateByID_EmailTaken_TokenInvalid(t *testing.T) {
	// A commit-time collision (person.ErrEmailTaken) collapses to the generic
	// ErrEmailChangeTokenInvalid, not 409.
	t.Parallel()

	newAddr, _ := email.New("rotated@example.test")
	p := newPersonWithPassword(t, "irrelevant")
	_ = b2SeedPendingEmailChange(t, p, newAddr) // arch-test:ignore-err - seed-only; plaintext not needed
	repo := newB2EmailPersonRepo(t, p)
	repo.errOnUpdateByID = person.ErrEmailTaken

	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{
		RawToken: "deterministic-test-token-base64url-stable-length-32+chars",
	})
	if !errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Fatalf("err = %v, want ErrEmailChangeTokenInvalid (person.ErrEmailTaken collapse)", err)
	}
}

func TestConfirmEmailChange_UpdateByID_OtherError_Wrapped(t *testing.T) {
	t.Parallel()

	newAddr, _ := email.New("rotated@example.test")
	p := newPersonWithPassword(t, "irrelevant")
	_ = b2SeedPendingEmailChange(t, p, newAddr) // arch-test:ignore-err - seed-only; plaintext not needed
	repo := newB2EmailPersonRepo(t, p)
	repo.errOnUpdateByID = errB2EmailChange

	h := command.NewConfirmEmailChangeHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ConfirmEmailChangeCommand{
		RawToken: "deterministic-test-token-base64url-stable-length-32+chars",
	})
	if !errors.Is(err, errB2EmailChange) {
		t.Fatalf("err = %v, want wrapped errB2EmailChange", err)
	}
	if errors.Is(err, command.ErrEmailChangeTokenInvalid) {
		t.Errorf("non-Invalid/EmailTaken infra error MUST NOT collapse to ErrEmailChangeTokenInvalid")
	}
}
