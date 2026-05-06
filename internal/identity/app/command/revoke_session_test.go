package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeFamilyRepo is the minimum [refreshtoken.Repository] surface
// the revoke-session handlers exercise.
type fakeFamilyRepo struct {
	families map[refreshtoken.FamilyID]*refreshtoken.Family
}

func newFakeFamilyRepo() *fakeFamilyRepo {
	return &fakeFamilyRepo{families: make(map[refreshtoken.FamilyID]*refreshtoken.Family)}
}

func (r *fakeFamilyRepo) Add(_ context.Context, f *refreshtoken.Family) error {
	r.families[f.ID()] = f
	return nil
}

func (r *fakeFamilyRepo) UpdateByID(_ context.Context, id refreshtoken.FamilyID, fn func(*refreshtoken.Family) (bool, error)) error {
	f, ok := r.families[id]
	if !ok {
		return refreshtoken.ErrNotFound
	}
	commit, err := fn(f)
	if err != nil {
		return err
	}
	_ = commit // commit semantics are repository-internal; no persistence in fake
	return nil
}

func (r *fakeFamilyRepo) GetByID(_ context.Context, id refreshtoken.FamilyID) (*refreshtoken.Family, error) {
	f, ok := r.families[id]
	if !ok {
		return nil, refreshtoken.ErrNotFound
	}
	return f, nil
}

func (r *fakeFamilyRepo) GetByTokenHash(_ context.Context, _ refreshtoken.TokenHash) (*refreshtoken.Family, error) {
	return nil, refreshtoken.ErrNotFound
}

func (r *fakeFamilyRepo) ListActiveForPerson(_ context.Context, pid person.ID) ([]*refreshtoken.Family, error) {
	var out []*refreshtoken.Family
	for _, f := range r.families {
		if f.PersonID() == pid && !f.IsRevoked() {
			out = append(out, f)
		}
	}
	return out, nil
}

var _ refreshtoken.Repository = (*fakeFamilyRepo)(nil)

func newFamily(t *testing.T, personID person.ID, deviceLabel string) *refreshtoken.Family {
	t.Helper()
	hash, err := refreshtoken.NewTokenHash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewTokenHashFromHex: %v", err)
	}
	f, err := refreshtoken.NewFamily(
		refreshtoken.FamilyID("11111111-1111-1111-1111-111111111111"),
		personID,
		tenant.ID("22222222-2222-2222-2222-222222222222"),
		deviceLabel,
		hash,
		14*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	return f
}

func setRevokeClock(t *testing.T) {
	t.Helper()
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)
}

// ----- RevokeSession ----------------------------------------------------------

func TestRevokeSession_Succeeds(t *testing.T) {
	t.Parallel()
	setRevokeClock(t)
	repo := newFakeFamilyRepo()
	pid := person.ID("p1")
	f := newFamily(t, pid, "iphone-15")
	_ = repo.Add(t.Context(), f)

	h := command.NewRevokeSessionHandler(repo)
	err := h.Handle(t.Context(), command.RevokeSessionCommand{
		PersonID: pid,
		FamilyID: f.ID(),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !f.IsRevoked() {
		t.Error("expected family revoked")
	}
	if f.RevokeReason() != "user_revoked" {
		t.Errorf("RevokeReason = %q, want 'user_revoked'", f.RevokeReason())
	}
}

func TestRevokeSession_CrossPerson_ReturnsNotFound(t *testing.T) {
	// Per security.md: "wrong owner" collapses to "not found" to defeat
	// FamilyID enumeration via ownership probing.
	t.Parallel()
	setRevokeClock(t)
	repo := newFakeFamilyRepo()
	owner := person.ID("p-owner")
	attacker := person.ID("p-attacker")
	f := newFamily(t, owner, "iphone-15")
	_ = repo.Add(t.Context(), f)

	h := command.NewRevokeSessionHandler(repo)
	err := h.Handle(t.Context(), command.RevokeSessionCommand{
		PersonID: attacker,
		FamilyID: f.ID(),
	})
	if !errors.Is(err, command.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
	if f.IsRevoked() {
		t.Error("attacker must NOT have revoked owner's family")
	}
}

func TestRevokeSession_AlreadyRevoked_IsIdempotent(t *testing.T) {
	t.Parallel()
	setRevokeClock(t)
	repo := newFakeFamilyRepo()
	pid := person.ID("p1")
	f := newFamily(t, pid, "iphone-15")
	if err := f.Revoke("logout"); err != nil {
		t.Fatalf("pre-revoke: %v", err)
	}
	_ = repo.Add(t.Context(), f)

	h := command.NewRevokeSessionHandler(repo)
	err := h.Handle(t.Context(), command.RevokeSessionCommand{
		PersonID: pid,
		FamilyID: f.ID(),
	})
	if err != nil {
		t.Fatalf("idempotent revoke should succeed; got %v", err)
	}
	if f.RevokeReason() != "logout" {
		t.Errorf("idempotent revoke must NOT overwrite reason; got %q", f.RevokeReason())
	}
}

func TestRevokeSession_NotFound_ReturnsErrSessionNotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeFamilyRepo()
	h := command.NewRevokeSessionHandler(repo)
	err := h.Handle(t.Context(), command.RevokeSessionCommand{
		PersonID: person.ID("p1"),
		FamilyID: refreshtoken.FamilyID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestRevokeSession_RejectsZeroPersonOrFamily(t *testing.T) {
	t.Parallel()
	repo := newFakeFamilyRepo()
	h := command.NewRevokeSessionHandler(repo)

	if err := h.Handle(t.Context(), command.RevokeSessionCommand{}); err == nil {
		t.Error("expected error on zero person + family")
	}
}

// ----- RevokeAllSessions ------------------------------------------------------

func TestRevokeAllSessions_RevokesAll(t *testing.T) {
	t.Parallel()
	setRevokeClock(t)
	repo := newFakeFamilyRepo()
	pid := person.ID("p1")
	f1 := newFamily(t, pid, "device-a")
	f2 := mustFamily(t, refreshtoken.FamilyID("33333333-3333-3333-3333-333333333333"), pid, "device-b")
	_ = repo.Add(t.Context(), f1)
	_ = repo.Add(t.Context(), f2)

	h := command.NewRevokeAllSessionsHandler(repo)
	out, err := h.Handle(t.Context(), command.RevokeAllSessionsCommand{
		PersonID: pid,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.RevokedCount != 2 {
		t.Errorf("RevokedCount = %d, want 2", out.RevokedCount)
	}
	if !f1.IsRevoked() || !f2.IsRevoked() {
		t.Error("both families should be revoked")
	}
}

func TestRevokeAllSessions_ExceptKeepsOneAlive(t *testing.T) {
	t.Parallel()
	setRevokeClock(t)
	repo := newFakeFamilyRepo()
	pid := person.ID("p1")
	current := newFamily(t, pid, "current-device")
	other := mustFamily(t, refreshtoken.FamilyID("33333333-3333-3333-3333-333333333333"), pid, "other-device")
	_ = repo.Add(t.Context(), current)
	_ = repo.Add(t.Context(), other)

	h := command.NewRevokeAllSessionsHandler(repo)
	out, err := h.Handle(t.Context(), command.RevokeAllSessionsCommand{
		PersonID:       pid,
		ExceptFamilyID: current.ID(),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.RevokedCount != 1 {
		t.Errorf("RevokedCount = %d, want 1", out.RevokedCount)
	}
	if current.IsRevoked() {
		t.Error("current device should still be active")
	}
	if !other.IsRevoked() {
		t.Error("other device should be revoked")
	}
}

func TestRevokeAllSessions_ZeroSessions_ReturnsZero(t *testing.T) {
	t.Parallel()
	repo := newFakeFamilyRepo()
	h := command.NewRevokeAllSessionsHandler(repo)
	out, err := h.Handle(t.Context(), command.RevokeAllSessionsCommand{
		PersonID: person.ID("p-no-sessions"),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.RevokedCount != 0 {
		t.Errorf("RevokedCount = %d, want 0", out.RevokedCount)
	}
}

func TestNewRevokeSessionHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil families repo")
		}
	}()
	_ = command.NewRevokeSessionHandler(nil)
}

func TestNewRevokeAllSessionsHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil families repo")
		}
	}()
	_ = command.NewRevokeAllSessionsHandler(nil)
}

func mustFamily(t *testing.T, id refreshtoken.FamilyID, personID person.ID, deviceLabel string) *refreshtoken.Family {
	t.Helper()
	hash, err := refreshtoken.NewTokenHash("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatalf("NewTokenHash: %v", err)
	}
	f, err := refreshtoken.NewFamily(
		id,
		personID,
		tenant.ID("22222222-2222-2222-2222-222222222222"),
		deviceLabel,
		hash,
		14*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	return f
}
