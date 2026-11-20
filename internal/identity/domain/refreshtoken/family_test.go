package refreshtoken_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// baseTime is the deterministic instant every refreshtoken test passes
// to factories + mutators per the clock-injection refactor.
var baseTime = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

func newFamilyID(t *testing.T) refreshtoken.FamilyID {
	t.Helper()
	return refreshtoken.FamilyID(ids.NewV7().String())
}

func newPersonID(t *testing.T) person.ID {
	t.Helper()
	return person.ID(ids.NewV7().String())
}

func newTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

func hash(s string) refreshtoken.TokenHash {
	// Test fixture — pretend SHA-256 of s. Real callers use crypto/sha256.
	// Length must be ≥ 40 chars to satisfy the validator. Pad to exactly
	// 64 chars (real SHA-256 hex length) so test data resembles production.
	const target = 64
	prefix := s + "_"
	padded := prefix + strings.Repeat("a", target-len(prefix))
	h, _ := refreshtoken.NewTokenHash(padded[:target])
	return h
}

// ----- Factory: NewFamily ---------------------------------------------------

func TestNewFamily_AcceptsValid(t *testing.T) {
	t.Parallel()

	id := newFamilyID(t)
	pid := newPersonID(t)
	tid := newTenantID(t)
	tokenHash := hash("first")

	f, err := refreshtoken.NewFamily(id, pid, tid, "Chrome on MacBook", tokenHash, 14*24*time.Hour, baseTime)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	if f == nil {
		t.Fatal("nil family")
	}
	if f.ID() != id {
		t.Errorf("ID() mismatch")
	}
	if f.PersonID() != pid {
		t.Errorf("PersonID mismatch")
	}
	if f.TenantID() != tid {
		t.Errorf("TenantID mismatch")
	}
	if f.DeviceLabel() != "Chrome on MacBook" {
		t.Errorf("DeviceLabel mismatch")
	}
	if f.IsRevoked() {
		t.Error("new family should not be revoked")
	}

	// Exactly one current token at generation 0.
	cur := f.CurrentToken()
	if cur == nil {
		t.Fatal("CurrentToken() = nil on fresh family")
	}
	if cur.Generation() != 0 {
		t.Errorf("first token generation = %d, want 0", cur.Generation())
	}
	if !cur.Hash().Equal(tokenHash) {
		t.Error("first token hash mismatch")
	}
}

func TestNewFamily_EmitsCreatedEvent(t *testing.T) {
	t.Parallel()

	f, err := refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), 14*24*time.Hour, baseTime)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	events := f.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(refreshtoken.FamilyCreatedEvent); !ok {
		t.Errorf("event[0] = %T, want FamilyCreatedEvent", events[0])
	}
}

func TestNewFamily_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		id          refreshtoken.FamilyID
		pid         person.ID
		tid         tenant.ID
		deviceLabel string
		hash        refreshtoken.TokenHash
	}{
		{"zero family id", refreshtoken.FamilyID(""), newPersonID(t), newTenantID(t), "Chrome", hash("a")},
		{"zero person id", newFamilyID(t), person.ID(""), newTenantID(t), "Chrome", hash("a")},
		{"zero tenant id", newFamilyID(t), newPersonID(t), tenant.ID(""), "Chrome", hash("a")},
		{"empty device", newFamilyID(t), newPersonID(t), newTenantID(t), "", hash("a")},
		{"zero hash", newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", refreshtoken.TokenHash{}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := refreshtoken.NewFamily(tc.id, tc.pid, tc.tid, tc.deviceLabel, tc.hash, 14*24*time.Hour, baseTime)
			if err == nil {
				t.Fatal("expected error")
			}
			if errs.KindOf(err) != errs.KindInvalidInput {
				t.Errorf("Kind = %v, want KindInvalidInput", errs.KindOf(err))
			}
		})
	}
}

func TestNewFamily_RejectsZeroOrNegativeTTL(t *testing.T) {
	t.Parallel()
	_, err := refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), 0, baseTime)
	if err == nil {
		t.Fatal("expected error on zero TTL")
	}
	_, err = refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), -time.Second, baseTime)
	if err == nil {
		t.Fatal("expected error on negative TTL")
	}
}

// ----- Rotate ---------------------------------------------------------------

func TestRotate_HappyPath_ConsumeCurrent_IssueNext(t *testing.T) {
	t.Parallel()

	f := newFamily(t)
	_ = f.PullEvents()

	original := f.CurrentToken()
	newHash := hash("second")
	rotateAt := baseTime.Add(1 * time.Hour)

	if err := f.Rotate(original.Hash(), newHash, 14*24*time.Hour, rotateAt); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Original should now be consumed.
	all := f.AllTokens()
	if len(all) != 2 {
		t.Fatalf("expected 2 tokens after rotation, got %d", len(all))
	}
	if all[0].ConsumedAt().IsZero() {
		t.Error("gen 0 should be consumed after rotation")
	}
	// New token is current.
	cur := f.CurrentToken()
	if cur.Generation() != 1 {
		t.Errorf("current gen = %d, want 1", cur.Generation())
	}
	if !cur.Hash().Equal(newHash) {
		t.Error("current hash mismatch")
	}
	if !all[0].ReplacedByID().Equal(cur.ID()) {
		t.Error("gen 0's ReplacedByID should reference gen 1")
	}

	// Event emitted.
	events := f.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(refreshtoken.RotatedEvent); !ok {
		t.Errorf("event[0] = %T, want RotatedEvent", events[0])
	}
}

func TestRotate_PresentingNonCurrentToken_DoesNothing_ReturnsError(t *testing.T) {
	t.Parallel()

	f := newFamily(t)
	_ = f.PullEvents()

	bogus := hash("not-in-family")
	err := f.Rotate(bogus, hash("new"), 14*24*time.Hour, baseTime)
	if err == nil {
		t.Fatal("expected ErrUnknownToken")
	}
	if !errors.Is(err, refreshtoken.ErrUnknownToken) {
		t.Errorf("expected ErrUnknownToken, got %v", err)
	}
	if f.IsRevoked() {
		t.Error("unknown token should NOT revoke family")
	}
}

func TestRotate_PresentingConsumedToken_RevokesFamily_ReuseDetected(t *testing.T) {
	t.Parallel()

	f := newFamily(t)
	_ = f.PullEvents()

	original := f.CurrentToken()
	if err := f.Rotate(original.Hash(), hash("second"), 14*24*time.Hour, baseTime); err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	_ = f.PullEvents()

	// Now an attacker (or a buggy client) presents the ALREADY CONSUMED gen-0
	// token. RFC 9700 §4.13: REUSE DETECTED → revoke entire family.
	err := f.Rotate(original.Hash(), hash("third"), 14*24*time.Hour, baseTime.Add(time.Second))
	if err == nil {
		t.Fatal("expected ErrReuseDetected")
	}
	if !errors.Is(err, refreshtoken.ErrReuseDetected) {
		t.Errorf("expected ErrReuseDetected, got %v", err)
	}
	if !f.IsRevoked() {
		t.Error("family should be revoked after reuse detection")
	}
	if f.RevokeReason() != "reuse_detected" {
		t.Errorf("RevokeReason = %q, want 'reuse_detected'", f.RevokeReason())
	}

	events := f.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(refreshtoken.RevokedEvent); !ok {
		t.Errorf("event[0] = %T, want RevokedEvent", events[0])
	}
}

func TestRotate_AfterRevoke_Fails(t *testing.T) {
	t.Parallel()

	f := newFamily(t)
	_ = f.PullEvents()
	_ = f.Revoke("user_revoked", baseTime) // arch-test:ignore-err - test fixture setup
	_ = f.PullEvents()

	err := f.Rotate(f.CurrentToken().Hash(), hash("new"), 14*24*time.Hour, baseTime)
	if err == nil {
		t.Fatal("expected error rotating revoked family")
	}
	if !errors.Is(err, refreshtoken.ErrRevoked) {
		t.Errorf("expected ErrRevoked, got %v", err)
	}
}

func TestRotate_AfterTokenExpiry_Fails(t *testing.T) {
	t.Parallel()

	f := newFamily(t)
	_ = f.PullEvents()

	// One month past the family's baseTime — well past the 14-day TTL.
	expired := baseTime.AddDate(0, 1, 0)
	err := f.Rotate(f.CurrentToken().Hash(), hash("new"), 14*24*time.Hour, expired)
	if err == nil {
		t.Fatal("expected error rotating expired token")
	}
	if !errors.Is(err, refreshtoken.ErrExpired) {
		t.Errorf("expected ErrExpired, got %v", err)
	}
}

// ----- Revoke ---------------------------------------------------------------

func TestRevoke_TransitionsToRevoked(t *testing.T) {
	t.Parallel()

	f := newFamily(t)
	_ = f.PullEvents()

	if err := f.Revoke("user_revoked", baseTime); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !f.IsRevoked() {
		t.Error("not revoked")
	}
	if f.RevokeReason() != "user_revoked" {
		t.Errorf("reason = %q", f.RevokeReason())
	}

	events := f.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestRevoke_RequiresReason(t *testing.T) {
	t.Parallel()
	f := newFamily(t)
	if err := f.Revoke("", baseTime); err == nil {
		t.Fatal("expected error on empty reason")
	}
}

func TestRevoke_Idempotent(t *testing.T) {
	t.Parallel()

	f := newFamily(t)
	_ = f.PullEvents()
	_ = f.Revoke("first", baseTime) // arch-test:ignore-err - test fixture setup
	_ = f.PullEvents()

	if err := f.Revoke("repeat", baseTime.Add(time.Second)); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	if got := f.PullEvents(); len(got) != 0 {
		t.Errorf("idempotent revoke emitted %d events, want 0", len(got))
	}
}

// ----- Re-hydration --------------------------------------------------------

func TestUnmarshalFromDB_DoesNotEmitEvents(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	f := refreshtoken.UnmarshalFromDB(refreshtoken.FamilySnapshot{
		ID:          newFamilyID(t),
		PersonID:    newPersonID(t),
		TenantID:    newTenantID(t),
		DeviceLabel: "Chrome",
		CreatedAt:   now,
		LastUsedAt:  now,
		Tokens: []refreshtoken.TokenSnapshot{
			{
				ID:         refreshtoken.TokenID(ids.NewV7().String()),
				Hash:       hash("a"),
				Generation: 0,
				IssuedAt:   now,
				ExpiresAt:  now.Add(14 * 24 * time.Hour),
			},
		},
	})
	if f == nil {
		t.Fatal("UnmarshalFromDB returned nil")
	}
	if got := f.PullEvents(); len(got) != 0 {
		t.Errorf("re-hydration emitted %d events", len(got))
	}
	if f.CurrentToken() == nil {
		t.Error("CurrentToken() should be reachable after re-hydration")
	}
}

// ----- Helpers --------------------------------------------------------------

func newFamily(t *testing.T) *refreshtoken.Family {
	t.Helper()
	f, err := refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), 14*24*time.Hour, baseTime)
	if err != nil {
		t.Fatalf("newFamily: %v", err)
	}
	return f
}

// ----- RotateWithGrace (ADR 0068 D10) ----------------------------------------

func TestRotateWithGrace(t *testing.T) {
	t.Parallel()
	const ttl, grace = 14 * 24 * time.Hour, 30 * time.Second
	t.Run("replay inside window returns same successor without change", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		_ = f.PullEvents()
		first := f.CurrentToken().Hash()
		if out, err := rotateGrace(f, first, hash("s1"), ttl, grace, baseTime); err != nil || out != refreshtoken.RotationIssued {
			t.Fatalf("rotate: %v %v", out, err)
		}
		_ = f.PullEvents()
		out, err := rotateGrace(f, first, hash("s1"), ttl, grace, baseTime.Add(10*time.Second))
		if err != nil || out != refreshtoken.RotationGraceReplay {
			t.Fatalf("replay: %v %v", out, err)
		}
		if f.IsRevoked() || len(f.AllTokens()) != 2 || len(f.PullEvents()) != 0 {
			t.Fatal("grace replay must not mutate")
		}
	})
	t.Run("replay exactly at the window edge is still a replay", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = rotateGrace(f, first, hash("s1"), ttl, grace, baseTime)
		if out, err := rotateGrace(f, first, hash("s1"), ttl, grace, baseTime.Add(grace)); err != nil || out != refreshtoken.RotationGraceReplay {
			t.Fatalf("edge: %v %v", out, err)
		}
	})
	t.Run("replay after window is reuse", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = rotateGrace(f, first, hash("s1"), ttl, grace, baseTime)
		if _, err := rotateGrace(f, first, hash("s1"), ttl, grace, baseTime.Add(31*time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) || !f.IsRevoked() {
			t.Fatalf("want reuse+revoke, got %v revoked=%v", err, f.IsRevoked())
		}
	})
	t.Run("replay after successor used is reuse", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = rotateGrace(f, first, hash("s1"), ttl, grace, baseTime)
		_, _ = rotateGrace(f, hash("s1"), hash("s2"), ttl, grace, baseTime.Add(time.Second))
		if _, err := rotateGrace(f, first, hash("s1"), ttl, grace, baseTime.Add(2*time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) {
			t.Fatalf("want reuse, got %v", err)
		}
	})
	t.Run("a token two generations old is reuse even inside its own window", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = rotateGrace(f, first, hash("s1"), ttl, grace, baseTime)
		_, _ = rotateGrace(f, hash("s1"), hash("s2"), ttl, grace, baseTime.Add(time.Second))
		// the candidate a deterministic derivation would produce for the old token
		if _, err := rotateGrace(f, first, hash("s1"), ttl, grace, baseTime.Add(time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) || !f.IsRevoked() {
			t.Fatalf("want reuse+revoke, got %v", err)
		}
	})
	t.Run("mismatched candidate is reuse", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = rotateGrace(f, first, hash("s1"), ttl, grace, baseTime)
		if _, err := rotateGrace(f, first, hash("other"), ttl, grace, baseTime.Add(time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) {
			t.Fatalf("want reuse, got %v", err)
		}
	})
	t.Run("a revoked family never replays", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = rotateGrace(f, first, hash("s1"), ttl, grace, baseTime)
		if err := f.Revoke("password-changed", baseTime.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		out, err := rotateGrace(f, first, hash("s1"), ttl, grace, baseTime.Add(2*time.Second))
		if !errors.Is(err, refreshtoken.ErrRevoked) || out == refreshtoken.RotationGraceReplay {
			t.Fatalf("revoked family: %v %v", out, err)
		}
		if f.RevokeReason() != "password-changed" {
			t.Fatalf("revocation reason overwritten: %q", f.RevokeReason())
		}
	})
	t.Run("zero grace is the strict rotation", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = rotateGrace(f, first, hash("s1"), ttl, 0, baseTime)
		if _, err := rotateGrace(f, first, hash("s1"), ttl, 0, baseTime); !errors.Is(err, refreshtoken.ErrReuseDetected) {
			t.Fatalf("want reuse, got %v", err)
		}
	})
}

// rotateGrace is one presentation without an absolute lifetime (the grace
// behaviour is what these tests are about).
func rotateGrace(f *refreshtoken.Family, presented, candidate refreshtoken.TokenHash, ttl, grace time.Duration, now time.Time) (refreshtoken.RotationOutcome, error) {
	return rotateAs(f, "client-A", presented, candidate, ttl, grace, now)
}

// rotateAs is one presentation by the named client binding.
func rotateAs(f *refreshtoken.Family, client string, presented, candidate refreshtoken.TokenHash, ttl, grace time.Duration, now time.Time) (refreshtoken.RotationOutcome, error) {
	return f.RotateWithGrace(refreshtoken.RotationParams{Presented: presented, Candidate: candidate, TTL: ttl, GraceWindow: grace, Client: client, Now: now})
}

// ----- Absolute lifetime (fix round 13, item 12) ------------------------------

func TestRotateWithGrace_AbsoluteLifetime(t *testing.T) {
	t.Parallel()
	const ttl, abs, grace = 14 * 24 * time.Hour, 24 * time.Hour, 30 * time.Second
	present := func(f *refreshtoken.Family, presented, candidate refreshtoken.TokenHash, at time.Time) (refreshtoken.RotationOutcome, error) {
		return f.RotateWithGrace(refreshtoken.RotationParams{Presented: presented, Candidate: candidate, TTL: ttl, AbsoluteTTL: abs, GraceWindow: grace, Now: at})
	}
	t.Run("one second before the deadline still rotates, and the new token never outlives it", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		if _, err := present(f, first, hash("s1"), baseTime.Add(abs-time.Second)); err != nil {
			t.Fatalf("rotate before the deadline: %v", err)
		}
		if got, want := f.CurrentToken().ExpiresAt(), baseTime.Add(abs); !got.Equal(want) {
			t.Fatalf("minted token expires %v, want clamped to the absolute deadline %v", got, want)
		}
	})
	t.Run("one second after the deadline is refused and revokes the family", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		_ = f.PullEvents()
		first := f.CurrentToken().Hash()
		_, err := present(f, first, hash("s1"), baseTime.Add(abs+time.Second))
		if !errors.Is(err, refreshtoken.ErrSessionExpired) {
			t.Fatalf("got %v, want ErrSessionExpired", err)
		}
		if !f.IsRevoked() || f.RevokeReason() != refreshtoken.RevokeReasonAbsoluteTTL {
			t.Fatalf("revoked=%v reason=%q, want revoked with absolute_ttl", f.IsRevoked(), f.RevokeReason())
		}
		if evs := f.PullEvents(); len(evs) != 1 {
			t.Fatalf("%d events, want exactly the revocation", len(evs))
		}
	})
	t.Run("a grace replay past the deadline is refused too", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		rotatedAt := baseTime.Add(abs - 5*time.Second)
		if _, err := present(f, first, hash("s1"), rotatedAt); err != nil {
			t.Fatal(err)
		}
		// Inside the 30 s window but 10 s past the deadline.
		if _, err := present(f, first, hash("s1"), rotatedAt.Add(15*time.Second)); !errors.Is(err, refreshtoken.ErrSessionExpired) || !f.IsRevoked() {
			t.Fatalf("replay past the deadline: %v revoked=%v", err, f.IsRevoked())
		}
	})
	t.Run("a revoked family keeps its own reason", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		if err := f.Revoke("logout", baseTime.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := present(f, first, hash("s1"), baseTime.Add(abs+time.Hour)); !errors.Is(err, refreshtoken.ErrRevoked) || f.RevokeReason() != "logout" {
			t.Fatalf("got %v reason %q", err, f.RevokeReason())
		}
	})
}

// The surviving mutant of the round-12 review: a replay inside the grace window is
// refused when its successor (the chain head) has itself already expired. With a
// token lifetime shorter than the window the head can be dead while the window is
// still open.
func TestRotateWithGrace_ExpiredHeadInsideTheWindowIsReuse(t *testing.T) {
	t.Parallel()
	const shortTTL, grace = 10 * time.Second, 30 * time.Second
	f := newFamily(t)
	first := f.CurrentToken().Hash()
	if _, err := f.RotateWithGrace(refreshtoken.RotationParams{Presented: first, Candidate: hash("s1"), TTL: shortTTL, GraceWindow: grace, Now: baseTime}); err != nil {
		t.Fatal(err)
	}
	// 20 s later: inside the 30 s window, past the successor's 10 s life.
	_, err := f.RotateWithGrace(refreshtoken.RotationParams{Presented: first, Candidate: hash("s1"), TTL: shortTTL, GraceWindow: grace, Now: baseTime.Add(20 * time.Second)})
	if !errors.Is(err, refreshtoken.ErrReuseDetected) || !f.IsRevoked() {
		t.Fatalf("replay onto an expired head: %v revoked=%v, want reuse + revoked", err, f.IsRevoked())
	}
}

// ----- Client binding and replay limit (fix round 13, item 3) ------------------

func TestRotateWithGrace_ClientBinding(t *testing.T) {
	t.Parallel()
	const ttl, grace = 14 * 24 * time.Hour, 30 * time.Second
	rotated := func(t *testing.T, client string) (*refreshtoken.Family, refreshtoken.TokenHash) {
		t.Helper()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		if _, err := rotateAs(f, client, first, hash("s1"), ttl, grace, baseTime); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		_ = f.PullEvents()
		return f, first
	}

	t.Run("the client that consumed the token is replayed up to the limit, then it is reuse", func(t *testing.T) {
		t.Parallel()
		f, first := rotated(t, "client-A")
		for i := 1; i <= refreshtoken.MaxGraceReplays; i++ {
			out, err := rotateAs(f, "client-A", first, hash("s1"), ttl, grace, baseTime.Add(time.Duration(i)*time.Second))
			if err != nil || out != refreshtoken.RotationGraceReplay {
				t.Fatalf("replay %d: %v %v", i, out, err)
			}
		}
		out, err := rotateAs(f, "client-A", first, hash("s1"), ttl, grace, baseTime.Add(5*time.Second))
		if !errors.Is(err, refreshtoken.ErrReuseDetected) || out != refreshtoken.RotationRefusedLimit || !f.IsRevoked() {
			t.Fatalf("replay %d: %v %v revoked=%v, want reuse / refused_limit / revoked", refreshtoken.MaxGraceReplays+1, out, err, f.IsRevoked())
		}
		if evs := f.PullEvents(); len(evs) != 1 {
			t.Fatalf("%d events, want exactly the revocation", len(evs))
		}
	})

	t.Run("another client inside the window is reuse and revokes the family", func(t *testing.T) {
		t.Parallel()
		f, first := rotated(t, "client-A")
		out, err := rotateAs(f, "client-B", first, hash("s1"), ttl, grace, baseTime.Add(time.Second))
		if !errors.Is(err, refreshtoken.ErrReuseDetected) || out != refreshtoken.RotationRefusedOtherClient || !f.IsRevoked() {
			t.Fatalf("other client: %v %v revoked=%v, want reuse / refused_other_client / revoked", out, err, f.IsRevoked())
		}
		if f.RevokeReason() != "reuse_detected" {
			t.Fatalf("revoke reason %q, want reuse_detected (the SIEM subscriber's trigger)", f.RevokeReason())
		}
		if evs := f.PullEvents(); len(evs) != 1 {
			t.Fatalf("%d events, want exactly the revocation", len(evs))
		}
	})

	t.Run("a token consumed without a binding is strict", func(t *testing.T) {
		t.Parallel()
		// Consumed by a request with no client, or before the binding existed (a NULL
		// column): nobody can prove they are the client that consumed it.
		for _, client := range []string{"", "client-A"} {
			g, firstG := rotated(t, "")
			if out, err := rotateAs(g, client, firstG, hash("s1"), ttl, grace, baseTime.Add(time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) || out != refreshtoken.RotationRefusedOtherClient {
				t.Fatalf("unbound token replayed by %q: %v %v, want reuse", client, out, err)
			}
		}
	})

	t.Run("the replay counter lives on the consumed token and is not bound by the grace of another", func(t *testing.T) {
		t.Parallel()
		f, first := rotated(t, "client-A")
		if _, err := rotateAs(f, "client-A", first, hash("s1"), ttl, grace, baseTime.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		toks := f.AllTokens()
		if toks[0].GraceReplays() != 1 || toks[1].GraceReplays() != 0 {
			t.Fatalf("replays: consumed=%d head=%d, want 1 and 0", toks[0].GraceReplays(), toks[1].GraceReplays())
		}
		if toks[0].ConsumedClient() != "client-A" || toks[1].ConsumedClient() != "" {
			t.Fatalf("bindings: consumed=%q head=%q, want client-A and empty", toks[0].ConsumedClient(), toks[1].ConsumedClient())
		}
	})
}

// ----- idle timeout + reuse after expiry (final fix round, commit 2 items 2 and 5) --

func TestRotateWithGrace_IdleTimeout(t *testing.T) {
	t.Parallel()
	const idle, abs = 2 * time.Hour, 14 * 24 * time.Hour
	newIdleFamily := func(t *testing.T) *refreshtoken.Family {
		t.Helper()
		f, err := refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), idle, baseTime)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	present := func(f *refreshtoken.Family, presented, candidate refreshtoken.TokenHash, at time.Time) (refreshtoken.RotationOutcome, error) {
		return f.RotateWithGrace(refreshtoken.RotationParams{Presented: presented, Candidate: candidate, TTL: idle, AbsoluteTTL: abs, GraceWindow: 30 * time.Second, Now: at})
	}

	t.Run("a token unused past its idle lifetime ends the session and revokes the family", func(t *testing.T) {
		t.Parallel()
		f := newIdleFamily(t)
		_ = f.PullEvents()
		_, err := present(f, f.CurrentToken().Hash(), hash("s1"), baseTime.Add(idle+time.Second))
		if !errors.Is(err, refreshtoken.ErrSessionExpired) || !errors.Is(err, refreshtoken.ErrExpired) {
			t.Fatalf("got %v, want ErrSessionExpired (and still ErrExpired)", err)
		}
		if !f.IsRevoked() || f.RevokeReason() != refreshtoken.RevokeReasonIdleTimeout {
			t.Fatalf("revoked=%v reason=%q, want revoked with idle_timeout", f.IsRevoked(), f.RevokeReason())
		}
		evs := f.PullEvents()
		if len(evs) != 1 {
			t.Fatalf("%d events, want exactly the revocation", len(evs))
		}
		if rev, ok := evs[0].(refreshtoken.RevokedEvent); !ok || rev.Reason != refreshtoken.RevokeReasonIdleTimeout {
			t.Fatalf("event %#v, want a RevokedEvent with idle_timeout", evs[0])
		}
	})
	t.Run("exactly at the idle deadline still rotates; the successor lives a fresh idle lifetime", func(t *testing.T) {
		t.Parallel()
		f := newIdleFamily(t)
		at := baseTime.Add(idle)
		if _, err := present(f, f.CurrentToken().Hash(), hash("s1"), at); err != nil {
			t.Fatalf("at the idle deadline: %v", err)
		}
		if got, want := f.CurrentToken().ExpiresAt(), at.Add(idle); !got.Equal(want) {
			t.Fatalf("successor expires %v, want %v (a rotation extends the idle deadline)", got, want)
		}
	})
	t.Run("a rotation extends the idle deadline but never the absolute one", func(t *testing.T) {
		t.Parallel()
		f := newIdleFamily(t)
		cur := f.CurrentToken().Hash()
		at := baseTime
		// Refresh every hour for 13 days and 23 hours: always inside the idle bound.
		for at.Add(time.Hour).Before(baseTime.Add(abs)) {
			at = at.Add(time.Hour)
			next := hash("s" + at.Format(time.RFC3339))
			if _, err := present(f, cur, next, at); err != nil {
				t.Fatalf("rotation at %v: %v", at, err)
			}
			cur = next
		}
		if got, want := f.CurrentToken().ExpiresAt(), baseTime.Add(abs); got.After(want) {
			t.Fatalf("token expires %v, past the absolute deadline %v", got, want)
		}
	})
	t.Run("a grace replay extends nothing", func(t *testing.T) {
		t.Parallel()
		f := newIdleFamily(t)
		first := f.CurrentToken().Hash()
		rotatedAt := baseTime.Add(time.Hour)
		if _, err := f.RotateWithGrace(refreshtoken.RotationParams{Presented: first, Candidate: hash("s1"), TTL: idle, AbsoluteTTL: abs, GraceWindow: 30 * time.Second, Client: "c", Now: rotatedAt}); err != nil {
			t.Fatal(err)
		}
		before := f.CurrentToken().ExpiresAt()
		out, err := f.RotateWithGrace(refreshtoken.RotationParams{Presented: first, Candidate: hash("s1"), TTL: idle, AbsoluteTTL: abs, GraceWindow: 30 * time.Second, Client: "c", Now: rotatedAt.Add(10 * time.Second)})
		if err != nil || out != refreshtoken.RotationGraceReplay {
			t.Fatalf("replay: %v %v", out, err)
		}
		if got := f.CurrentToken().ExpiresAt(); !got.Equal(before) {
			t.Fatalf("a grace replay moved the idle deadline from %v to %v", before, got)
		}
	})
}

func TestRotateWithGrace_ConsumedTokenAfterTheDeadlineIsReuse(t *testing.T) {
	t.Parallel()
	const ttl, abs, grace = 14 * 24 * time.Hour, 24 * time.Hour, 30 * time.Second
	present := func(f *refreshtoken.Family, presented, candidate refreshtoken.TokenHash, client string, at time.Time) (refreshtoken.RotationOutcome, error) {
		return f.RotateWithGrace(refreshtoken.RotationParams{Presented: presented, Candidate: candidate, TTL: ttl, AbsoluteTTL: abs, GraceWindow: grace, Client: client, Now: at})
	}
	t.Run("an old consumed token presented after the deadline is recorded as reuse", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		if _, err := present(f, first, hash("s1"), "c", baseTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		_ = f.PullEvents()
		_, err := present(f, first, hash("s1"), "thief", baseTime.Add(abs+time.Hour))
		if !errors.Is(err, refreshtoken.ErrReuseDetected) || !errors.Is(err, refreshtoken.ErrSessionExpired) {
			t.Fatalf("got %v, want ErrReuseDetected and ErrSessionExpired", err)
		}
		if !f.IsRevoked() || f.RevokeReason() != refreshtoken.RevokeReasonReuseAfterExpiry {
			t.Fatalf("revoked=%v reason=%q, want reuse_after_expiry", f.IsRevoked(), f.RevokeReason())
		}
		evs := f.PullEvents()
		if len(evs) != 1 {
			t.Fatalf("%d events, want the one revocation", len(evs))
		}
		if rev, ok := evs[0].(refreshtoken.RevokedEvent); !ok || rev.Reason != refreshtoken.RevokeReasonReuseAfterExpiry {
			t.Fatalf("event %#v, want a RevokedEvent with reuse_after_expiry", evs[0])
		}
	})
	t.Run("the CURRENT token after the deadline stays a plain absolute expiry", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		_, err := present(f, f.CurrentToken().Hash(), hash("s1"), "c", baseTime.Add(abs+time.Second))
		if !errors.Is(err, refreshtoken.ErrSessionExpired) || errors.Is(err, refreshtoken.ErrReuseDetected) || f.RevokeReason() != refreshtoken.RevokeReasonAbsoluteTTL {
			t.Fatalf("got %v reason %q, want ErrSessionExpired with absolute_ttl and no reuse", err, f.RevokeReason())
		}
	})
}
