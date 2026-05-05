package jwt_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
)

// fixedClock returns the same instant on every call — required so the
// jwtv5 parser's exp + nbf checks are deterministic against TTL constants.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

const (
	hex32a = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hex32b = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func issuerWithSecret(t *testing.T, kid, secretHex string, now time.Time) *jwt.Issuer {
	t.Helper()
	iss, err := jwt.NewIssuer(jwt.SigningKey{KeyID: kid, Secret: []byte(secretHex)}, nil, fixedClock(now))
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

func validArgs() jwt.IssueArgs {
	return jwt.IssueArgs{
		PersonID:      "12345678-1234-1234-1234-123456789012",
		TenantID:      "abcdef01-2345-6789-abcd-ef0123456789",
		TenantSlug:    "acme",
		MembershipID:  "deadbeef-1234-5678-9abc-def012345678",
		SecurityStamp: "0123beef-2345-6789-abcd-ef0123456789",
		IsPlatform:    false,
		IsSuperUser:   false,
		Permissions:   []string{"crm.leads.view", "crm.leads.create"},
	}
}

func TestIssue_RoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	iss := issuerWithSecret(t, "k1", hex32a, now)

	tok, err := iss.Issue(validArgs())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !strings.HasPrefix(tok, "eyJ") {
		t.Fatalf("not a JWT: %s", tok)
	}

	claims, err := iss.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != validArgs().PersonID {
		t.Fatalf("sub: got %q want %q", claims.Subject, validArgs().PersonID)
	}
	if claims.TenantID != validArgs().TenantID {
		t.Fatalf("tenant_id: got %q want %q", claims.TenantID, validArgs().TenantID)
	}
	if claims.SecurityStamp != validArgs().SecurityStamp {
		t.Fatalf("security_stamp round-trip mismatch")
	}
	if len(claims.Permissions) != 2 {
		t.Fatalf("permissions count: got %d want 2", len(claims.Permissions))
	}
	if claims.ID == "" {
		t.Fatal("jti not set")
	}
}

func TestVerify_TamperedSignature_RejectedAsErrInvalidToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	iss := issuerWithSecret(t, "k1", hex32a, now)

	tok, _ := iss.Issue(validArgs())
	tampered := tok[:len(tok)-4] + "AAAA"
	_, err := iss.Verify(tampered)
	if !errors.Is(err, jwt.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestVerify_Expired_RejectedAsErrInvalidToken(t *testing.T) {
	t.Parallel()
	issuedAt := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	iss := issuerWithSecret(t, "k1", hex32a, issuedAt)
	tok, _ := iss.Issue(validArgs())

	// Verify in the future, past TTL + leeway.
	future := issuedAt.Add(jwt.AccessTokenTTL + jwt.ClockSkew + time.Second)
	verifier, _ := jwt.NewIssuer(
		jwt.SigningKey{KeyID: "k1", Secret: []byte(hex32a)}, nil, fixedClock(future),
	)
	_, err := verifier.Verify(tok)
	if !errors.Is(err, jwt.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken on expired, got %v", err)
	}
}

func TestVerify_KeyRotation_AcceptsPreviousKid(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	oldIss := issuerWithSecret(t, "k-old", hex32a, now)
	tok, _ := oldIss.Issue(validArgs())

	// New issuer rotated to k-new; k-old still accepted in PreviousSigningKeys.
	rotated, err := jwt.NewIssuer(
		jwt.SigningKey{KeyID: "k-new", Secret: []byte(hex32b)},
		[]jwt.SigningKey{{KeyID: "k-old", Secret: []byte(hex32a)}},
		fixedClock(now),
	)
	if err != nil {
		t.Fatalf("NewIssuer rotated: %v", err)
	}
	if _, err := rotated.Verify(tok); err != nil {
		t.Fatalf("rotated issuer rejected old-kid token: %v", err)
	}
}

func TestVerify_UnknownKid_Rejected(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	iss := issuerWithSecret(t, "k-old", hex32a, now)
	tok, _ := iss.Issue(validArgs())

	// Verifier with NO previous-key history; should reject.
	verifier := issuerWithSecret(t, "k-new", hex32b, now)
	_, err := verifier.Verify(tok)
	if !errors.Is(err, jwt.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken on unknown kid, got %v", err)
	}
}

func TestNewIssuer_RejectsShortSecret(t *testing.T) {
	t.Parallel()
	_, err := jwt.NewIssuer(jwt.SigningKey{KeyID: "k", Secret: []byte("short")}, nil, time.Now)
	if err == nil {
		t.Fatal("expected error on <32-byte secret")
	}
}

func TestIssue_RequiresPersonID(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	iss := issuerWithSecret(t, "k1", hex32a, now)
	args := validArgs()
	args.PersonID = ""
	if _, err := iss.Issue(args); err == nil {
		t.Fatal("expected error on empty PersonID")
	}
}
