package refreshtoken_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

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

func freezeClock(t *testing.T, base time.Time) {
	t.Helper()
	clock.Set(base)
	t.Cleanup(clock.Reset)
}

// ----- Factory: NewFamily ---------------------------------------------------

func TestNewFamily_AcceptsValid(t *testing.T) {
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	id := newFamilyID(t)
	pid := newPersonID(t)
	tid := newTenantID(t)
	tokenHash := hash("first")

	f, err := refreshtoken.NewFamily(id, pid, tid, "Chrome on MacBook", tokenHash, 14*24*time.Hour)
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
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f, err := refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), 14*24*time.Hour)
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
			_, err := refreshtoken.NewFamily(tc.id, tc.pid, tc.tid, tc.deviceLabel, tc.hash, 14*24*time.Hour)
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
	_, err := refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), 0)
	if err == nil {
		t.Fatal("expected error on zero TTL")
	}
	_, err = refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), -time.Second)
	if err == nil {
		t.Fatal("expected error on negative TTL")
	}
}

// ----- Rotate ---------------------------------------------------------------

func TestRotate_HappyPath_ConsumeCurrent_IssueNext(t *testing.T) {
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f := newFamily(t)
	_ = f.PullEvents()

	clock.Set(time.Date(2026, 5, 5, 13, 0, 0, 0, time.UTC))
	original := f.CurrentToken()
	newHash := hash("second")

	if err := f.Rotate(original.Hash(), newHash, 14*24*time.Hour); err != nil {
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
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f := newFamily(t)
	_ = f.PullEvents()

	bogus := hash("not-in-family")
	err := f.Rotate(bogus, hash("new"), 14*24*time.Hour)
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
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f := newFamily(t)
	_ = f.PullEvents()

	original := f.CurrentToken()
	if err := f.Rotate(original.Hash(), hash("second"), 14*24*time.Hour); err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	_ = f.PullEvents()

	// Now an attacker (or a buggy client) presents the ALREADY CONSUMED gen-0
	// token. RFC 9700 §4.13: REUSE DETECTED → revoke entire family.
	err := f.Rotate(original.Hash(), hash("third"), 14*24*time.Hour)
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
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f := newFamily(t)
	_ = f.PullEvents()
	_ = f.Revoke("user_revoked")
	_ = f.PullEvents()

	err := f.Rotate(f.CurrentToken().Hash(), hash("new"), 14*24*time.Hour)
	if err == nil {
		t.Fatal("expected error rotating revoked family")
	}
	if !errors.Is(err, refreshtoken.ErrRevoked) {
		t.Errorf("expected ErrRevoked, got %v", err)
	}
}

func TestRotate_AfterTokenExpiry_Fails(t *testing.T) {
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f := newFamily(t)
	_ = f.PullEvents()

	// Advance past TTL.
	clock.Set(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	err := f.Rotate(f.CurrentToken().Hash(), hash("new"), 14*24*time.Hour)
	if err == nil {
		t.Fatal("expected error rotating expired token")
	}
	if !errors.Is(err, refreshtoken.ErrExpired) {
		t.Errorf("expected ErrExpired, got %v", err)
	}
}

// ----- Revoke ---------------------------------------------------------------

func TestRevoke_TransitionsToRevoked(t *testing.T) {
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f := newFamily(t)
	_ = f.PullEvents()

	if err := f.Revoke("user_revoked"); err != nil {
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
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))
	f := newFamily(t)
	if err := f.Revoke(""); err == nil {
		t.Fatal("expected error on empty reason")
	}
}

func TestRevoke_Idempotent(t *testing.T) {
	freezeClock(t, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	f := newFamily(t)
	_ = f.PullEvents()
	_ = f.Revoke("first")
	_ = f.PullEvents()

	if err := f.Revoke("repeat"); err != nil {
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
	f, err := refreshtoken.NewFamily(newFamilyID(t), newPersonID(t), newTenantID(t), "Chrome", hash("a"), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("newFamily: %v", err)
	}
	return f
}
