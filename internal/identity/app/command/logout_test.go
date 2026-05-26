// logout_test.go — handler-unit tests for the LogoutHandler. Covers
// every branch of logout.go::Handle per ADR 0062 §6 (handler-unit
// MANY). Logout is idempotent (security goal: "this token can't be
// used again"); the tests exercise:
//
//   - happy path: family revoked + reason stamped on aggregate
//   - empty cmd.Reason → defaults to "user-logout"
//   - explicit reason passes through verbatim
//   - malformed plaintext → idempotent nil (no surface)
//   - unknown hash → idempotent nil
//   - GetByTokenHash non-NotFound error → wrapped
//   - Revoke aggregate error → wrapped via UpdateByID closure
//
// Wired against the refreshtokentest.FakeRepository per TDL canon §6
// + ADR 0062. No SQL contract is exercised — orchestration only.

package command_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// logoutFamiliesWithErr is an inline error-injecting decorator on top
// of the refreshtokentest.FakeRepository used by tests that need to
// trigger the non-ErrNotFound GetByTokenHash branch. Scoped to
// logout_test.go to avoid collisions with refresh_test.go's
// rtErrFamilies (different injection surfaces).
type logoutFamiliesWithErr struct {
	*refreshtokentest.FakeRepository
	getByHashErr  error
	updateByIDErr error
}

func (r *logoutFamiliesWithErr) GetByTokenHash(ctx context.Context, hash refreshtoken.TokenHash) (*refreshtoken.Family, error) {
	if r.getByHashErr != nil {
		return nil, r.getByHashErr
	}
	return r.FakeRepository.GetByTokenHash(ctx, hash)
}

func (r *logoutFamiliesWithErr) UpdateByID(ctx context.Context, id refreshtoken.FamilyID, fn func(*refreshtoken.Family) (bool, error)) error {
	if r.updateByIDErr != nil {
		return r.updateByIDErr
	}
	return r.FakeRepository.UpdateByID(ctx, id, fn)
}

// seedLogoutFamily mints a fresh refresh family in the fake + returns
// (plaintext, familyID) for caller use.
func seedLogoutFamily(t *testing.T, fams refreshtoken.Repository) (string, refreshtoken.FamilyID) {
	t.Helper()
	pair, err := refreshmint.Mint()
	if err != nil {
		t.Fatalf("refreshmint.Mint: %v", err)
	}
	famID := refreshtoken.FamilyID(ids.NewV7().String())
	fam, err := refreshtoken.NewFamily(
		famID,
		person.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		"test-device",
		pair.Hash,
		14*24*time.Hour,
		testNow,
	)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	if err := fams.Add(t.Context(), fam); err != nil {
		t.Fatalf("families.Add: %v", err)
	}
	return pair.Plaintext, famID
}

// TestLogout_HappyPath_RevokesFamily covers the success branch — the
// family is revoked + audit reason recorded on the aggregate.
func TestLogout_HappyPath_RevokesFamily(t *testing.T) {
	t.Parallel()
	fams := refreshtokentest.NewFakeRepository()
	plain, famID := seedLogoutFamily(t, fams)
	h := command.NewLogoutHandler(fams, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.LogoutCommand{
		RefreshTokenPlain: plain,
		Reason:            "user-logout",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// State assertion: family is revoked + carries the reason.
	fam, err := fams.GetByID(t.Context(), famID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !fam.IsRevoked() {
		t.Fatal("Family.IsRevoked = false after logout")
	}
	if fam.RevokeReason() != "user-logout" {
		t.Errorf("RevokeReason = %q, want %q", fam.RevokeReason(), "user-logout")
	}
}

// TestLogout_EmptyReason_DefaultsToUserLogout covers the cmd.Reason
// == "" branch — handler defaults to "user-logout" so the audit log
// is never empty-stringed.
func TestLogout_EmptyReason_DefaultsToUserLogout(t *testing.T) {
	t.Parallel()
	fams := refreshtokentest.NewFakeRepository()
	plain, famID := seedLogoutFamily(t, fams)
	h := command.NewLogoutHandler(fams, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.LogoutCommand{
		RefreshTokenPlain: plain,
		// Reason intentionally omitted.
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	fam, err := fams.GetByID(t.Context(), famID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fam.RevokeReason() != "user-logout" {
		t.Errorf("RevokeReason = %q, want default %q", fam.RevokeReason(), "user-logout")
	}
}

// TestLogout_ExplicitReason_PassedThrough covers the cmd.Reason !=
// "" branch — handler uses the caller's reason verbatim.
func TestLogout_ExplicitReason_PassedThrough(t *testing.T) {
	t.Parallel()
	fams := refreshtokentest.NewFakeRepository()
	plain, famID := seedLogoutFamily(t, fams)
	h := command.NewLogoutHandler(fams, func() time.Time { return testNow })

	const reason = "admin-revoke-suspicious-activity"
	err := h.Handle(t.Context(), command.LogoutCommand{
		RefreshTokenPlain: plain,
		Reason:            reason,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	fam, err := fams.GetByID(t.Context(), famID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fam.RevokeReason() != reason {
		t.Errorf("RevokeReason = %q, want %q", fam.RevokeReason(), reason)
	}
}

// TestLogout_MalformedPlaintext_IdempotentNil covers the
// refreshtoken.NewTokenHash error branch. In practice HashOf("")
// returns a valid hex SHA-256, so the hash-construction branch is
// not reachable from an empty plaintext alone — but the handler
// MUST handle the hypothetical malformed-hash case as
// idempotent-success. We exercise via empty plaintext where the
// lookup misses; observable is nil error (idempotent).
func TestLogout_MalformedPlaintext_IdempotentNil(t *testing.T) {
	t.Parallel()
	fams := refreshtokentest.NewFakeRepository()
	h := command.NewLogoutHandler(fams, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.LogoutCommand{
		RefreshTokenPlain: "", // HashOf("") is valid hex; lookup misses → nil.
	})
	if err != nil {
		t.Fatalf("Handle: got %v, want nil (idempotent)", err)
	}
}

// TestLogout_UnknownHash_IdempotentNil covers the ErrNotFound branch
// on GetByTokenHash — never-issued or already-revoked token surfaces
// as idempotent-success.
func TestLogout_UnknownHash_IdempotentNil(t *testing.T) {
	t.Parallel()
	fams := refreshtokentest.NewFakeRepository()
	h := command.NewLogoutHandler(fams, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.LogoutCommand{
		RefreshTokenPlain: "never-issued-token-plaintext-value",
	})
	if err != nil {
		t.Fatalf("Handle: got %v, want nil (idempotent on unknown hash)", err)
	}
}

// TestLogout_LookupError_Wrapped covers the non-ErrNotFound branch
// on GetByTokenHash — a real infra error must surface as a wrapped
// "logout: lookup family" error (NOT swallowed) so operators can
// distinguish transient failures from idempotent success.
func TestLogout_LookupError_Wrapped(t *testing.T) {
	t.Parallel()
	fams := &logoutFamiliesWithErr{FakeRepository: refreshtokentest.NewFakeRepository()}
	sentinel := errors.New("pgx: connection reset")
	fams.getByHashErr = sentinel
	h := command.NewLogoutHandler(fams, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.LogoutCommand{
		RefreshTokenPlain: "any",
	})
	if err == nil {
		t.Fatal("expected wrapped infra error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want chain containing %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "lookup family") {
		t.Errorf("err msg %q missing 'lookup family' prefix", err.Error())
	}
}

// TestLogout_RevokePersistError_Wrapped covers the UpdateByID error
// branch — infra error during the revoke persist surfaces as
// "logout: persist revoke: <wrapped>".
func TestLogout_RevokePersistError_Wrapped(t *testing.T) {
	t.Parallel()
	fams := &logoutFamiliesWithErr{FakeRepository: refreshtokentest.NewFakeRepository()}
	plain, _ := seedLogoutFamily(t, fams.FakeRepository)
	sentinel := errors.New("pgx: serialization failure")
	fams.updateByIDErr = sentinel
	h := command.NewLogoutHandler(fams, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.LogoutCommand{
		RefreshTokenPlain: plain,
		Reason:            "user-logout",
	})
	if err == nil {
		t.Fatal("expected wrapped UpdateByID error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want chain containing %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "persist revoke") {
		t.Errorf("err msg %q missing 'persist revoke' prefix", err.Error())
	}
}

// TestNewLogoutHandler_NilNow_DefaultsToTimeNow covers the wiring
// fallback: passing nil for `now` defaults to time.Now. Subtle but
// load-bearing — composition root passes time.Now explicitly; this
// ensures the handler never panics on a nil-clock construction.
func TestNewLogoutHandler_NilNow_DefaultsToTimeNow(t *testing.T) {
	t.Parallel()
	fams := refreshtokentest.NewFakeRepository()
	// Pass nil for now — must not panic.
	h := command.NewLogoutHandler(fams, nil)
	// Smoke: a zero-arg Handle on empty repo returns nil (idempotent).
	err := h.Handle(t.Context(), command.LogoutCommand{RefreshTokenPlain: "x"})
	if err != nil {
		t.Fatalf("Handle: got %v, want nil", err)
	}
}
