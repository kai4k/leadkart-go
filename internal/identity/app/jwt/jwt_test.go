package jwt_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"

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

func TestIssue_RankClaim_RoundTripsAndStaysNilWhenAbsent(t *testing.T) {
	t.Parallel()
	iss := issuerWithSecret(t, "k1", hex32a, time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC))
	for _, want := range []int{0, 20, 99} {
		args := validArgs()
		args.Rank = &want
		tok, err := iss.Issue(args)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		c, err := iss.Verify(tok)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if c.Rank == nil || *c.Rank != want {
			t.Fatalf("rank round-trip: got %v want %d", c.Rank, want)
		}
	}
	tok, err := iss.Issue(validArgs())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c, err := iss.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Rank != nil {
		t.Fatalf("absent rank must stay nil (never 0 = owner), got %d", *c.Rank)
	}
}

func TestIssue_MustChangePasswordClaim_RoundTripsAndDefaultsFalse(t *testing.T) {
	t.Parallel()
	iss := issuerWithSecret(t, "k1", hex32a, time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC))

	args := validArgs()
	args.MustChangePassword = true
	tok, err := iss.Issue(args)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c, err := iss.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !c.MustChangePassword {
		t.Fatal("must_change_password claim lost in round trip")
	}

	// A token minted without the flag (every token minted before the claim
	// existed) verifies as un-flagged.
	tok, err = iss.Issue(validArgs())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c, err = iss.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.MustChangePassword {
		t.Fatal("absent claim must verify as false")
	}
}

// ADR 0045 / plan D9: the impersonation audience and the act claim travel
// together. A token with one and not the other is never valid.
func TestVerify_AudienceAndActClaimMustAgree(t *testing.T) {
	t.Parallel()
	iss := issuerWithSecret(t, "k1", hex32a, time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC))
	act := &jwt.ActClaim{Sub: "12345678-1234-1234-1234-123456789012", SessionID: "s1", Reason: "support ticket"}

	cases := []struct {
		name     string
		audience string
		act      *jwt.ActClaim
		ok       bool
	}{
		{"ordinary token", "", nil, true},
		{"impersonation token", jwt.ImpersonationAudienceClaim, act, true},
		{"impersonation audience without act", jwt.ImpersonationAudienceClaim, nil, false},
		{"ordinary audience with act", jwt.AudienceClaim, act, false},
		{"default audience with act", "", act, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := validArgs()
			args.Audience = tc.audience
			args.Act = tc.act
			tok, err := iss.Issue(args)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			_, err = iss.Verify(tok)
			if tc.ok && err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !tc.ok && !errors.Is(err, jwt.ErrInvalidToken) {
				t.Fatalf("Verify err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// signRaw hand-builds and signs a token, because Issue cannot produce malformed
// audience / act shapes. The claims map is exactly what goes on the wire.
func signRaw(t *testing.T, now time.Time, extra map[string]any) string {
	t.Helper()
	claims := jwtv5.MapClaims{
		"iss":            jwt.IssuerClaim,
		"sub":            "12345678-1234-1234-1234-123456789012",
		"iat":            now.Unix(),
		"nbf":            now.Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"tenant_id":      "abcdef01-2345-6789-abcd-ef0123456789",
		"security_stamp": "0123beef-2345-6789-abcd-ef0123456789",
		"membership_id":  "deadbeef-1234-5678-9abc-def012345678",
		"tenant_slug":    "acme",
		"jti":            "raw-token-1",
	}
	for k, v := range extra {
		if v == absent {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString([]byte(hex32a))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// absent marks a claim to delete from the wire form.
const absent = "\x00absent"

// The audience and act rules, asserted on the exact wire shapes an attacker (or a
// buggy issuer) could produce. Each case states its outcome: a rejection is
// ErrInvalidToken; the one accepted malformed-looking shape (act: null on an
// ordinary token) verifies as a token with no act.
func TestVerify_RawAudienceAndActShapes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	iss := issuerWithSecret(t, "k1", hex32a, now)
	fullAct := map[string]any{"sub": "12345678-1234-1234-1234-123456789012", "session_id": "s1", "reason": "support ticket"}

	cases := []struct {
		name      string
		claims    map[string]any
		wantOK    bool
		wantNoAct bool
	}{
		{"aud array of two values", map[string]any{"aud": []string{jwt.AudienceClaim, jwt.ImpersonationAudienceClaim}, "act": fullAct}, false, false},
		{"aud array of two ordinary values", map[string]any{"aud": []string{jwt.AudienceClaim, jwt.AudienceClaim}}, false, false},
		{"aud missing", map[string]any{"aud": absent}, false, false},
		{"aud empty string", map[string]any{"aud": ""}, false, false},
		{"aud empty array", map[string]any{"aud": []string{}}, false, false},
		{"aud wrong case", map[string]any{"aud": "LeadKart-API"}, false, false},
		{"impersonation aud wrong case", map[string]any{"aud": "Leadkart-Impersonation", "act": fullAct}, false, false},
		{"ordinary aud with empty act object", map[string]any{"aud": jwt.AudienceClaim, "act": map[string]any{}}, false, false},
		{"impersonation aud with empty act object", map[string]any{"aud": jwt.ImpersonationAudienceClaim, "act": map[string]any{}}, false, false},
		{"impersonation aud with act missing its session", map[string]any{"aud": jwt.ImpersonationAudienceClaim, "act": map[string]any{"sub": "12345678-1234-1234-1234-123456789012"}}, false, false},
		{"ordinary aud with act null", map[string]any{"aud": jwt.AudienceClaim, "act": nil}, true, true},
		{"impersonation aud with act null", map[string]any{"aud": jwt.ImpersonationAudienceClaim, "act": nil}, false, false},
		{"impersonation aud, no act claim at all", map[string]any{"aud": jwt.ImpersonationAudienceClaim}, false, false},
		{"ordinary aud with a full act", map[string]any{"aud": jwt.AudienceClaim, "act": fullAct}, false, false},
		{"well-formed impersonation token", map[string]any{"aud": jwt.ImpersonationAudienceClaim, "act": fullAct}, true, false},
		{"well-formed ordinary token", map[string]any{"aud": jwt.AudienceClaim}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := iss.Verify(signRaw(t, now, tc.claims))
			if !tc.wantOK {
				if !errors.Is(err, jwt.ErrInvalidToken) {
					t.Fatalf("Verify err = %v, want ErrInvalidToken", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if tc.wantNoAct && c.Act != nil {
				t.Fatalf("act = %+v, want nil", c.Act)
			}
		})
	}
}
