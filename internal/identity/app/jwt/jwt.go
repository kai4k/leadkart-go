// Package jwt issues + verifies LeadKart access tokens.
//
// Per LeadKart .NET security.md "Access token" + "JWT signing key" canon
// + RFC 7515 (JWS) + RFC 7517 (JWK) + RFC 8725 (JWT BCP) + RFC 9700
// (OAuth 2.0 Security BCP):
//
//   - 10-minute access-token lifetime; ClockSkew 60s.
//   - Required claims: sub (PersonId — global identity, NEVER changes
//     during session), tenant_id, tenant_slug, membership_id,
//     security_stamp, is_platform, is_super_user, jti.
//   - HMAC-SHA256 (HS256) signing — single-key rotation via kid header
//   - accepted PreviousSigningKeys list.
//
// JWT is the access-token-only path. Refresh tokens are opaque + stored
// hash-only per [refreshtoken.Family] — issued + verified by
// [internal/identity/app/refreshmint], not this package.
package jwt

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// AccessTokenTTL is the per-token absolute expiry — 10 min per security.md.
const AccessTokenTTL = 10 * time.Minute

// ClockSkew is the leeway granted on exp/nbf comparison — 60 sec per
// security.md (Auth0/Okta canon — tolerate small server-clock drift).
const ClockSkew = 60 * time.Second

// IssuerClaim + AudienceClaim are the wire-stable iss + aud values
// pinned per RFC 7519 §4.1.1 (iss) + §4.1.3 (aud) and required by
// RFC 8725 §3.10/§3.11 ("Always Validate Issuer and Audience"). A
// token signed by a different LeadKart deployment / environment /
// sibling service that happens to share the HS256 secret would
// otherwise verify here — pinning iss + aud to LeadKart's own
// identifier closes the cross-environment confusion vector.
//
// Named *Claim (vs Issuer / Audience plain) so they don't clash
// with the [Issuer] struct type.
//
// ImpersonationAudienceClaim distinguishes scoped impersonation
// tokens (per ADR 0045) from regular API tokens. Both audiences are
// accepted by [Verify]; the audience is preserved in the parsed
// claims so route-level middleware can REJECT impersonation tokens
// at endpoints that shouldn't accept them (e.g. opening a sub-
// impersonation from inside an impersonation session).
const (
	IssuerClaim                = "leadkart-identity"
	AudienceClaim              = "leadkart-api"
	ImpersonationAudienceClaim = "leadkart-impersonation"
)

// ErrInvalidToken is returned by [Issuer.Verify] when the token is
// expired, signature-invalid, kid-unknown, or any other shape failure.
// Caller maps to HTTP 401; intentionally generic to avoid leaking which
// failure mode hit.
var ErrInvalidToken = errors.New("jwt: invalid token")

// SigningKey carries one key plus its kid header value. Multi-key
// rotation per security.md "Key history" — Issuer holds the current
// key + accepts a list of previous keys for verification only.
type SigningKey struct {
	KeyID  string // emitted as `kid` JWT header, used to route on Verify
	Secret []byte // HS256 secret — minimum 32 bytes per RFC 7518 §3.2
}

// Claims is the LeadKart-shaped JWT payload. Marshals into the standard
// `RegisteredClaims` JSON object (jti=Iss, sub, exp, nbf, iat) plus our
// custom claims with snake_case names matching the .NET version.
//
// Act is the RFC 8693 §4.1 actor claim — non-nil ONLY for scoped
// impersonation tokens per ADR 0045. When set, it identifies the
// ORIGINAL operator (the one who started the impersonation session);
// Subject + TenantID identify the IMPERSONATED context.
type Claims struct {
	jwtv5.RegisteredClaims

	TenantID      string   `json:"tenant_id"`
	TenantSlug    string   `json:"tenant_slug"`
	MembershipID  string   `json:"membership_id"`
	SecurityStamp string   `json:"security_stamp"`
	IsPlatform    bool     `json:"is_platform"`
	IsSuperUser   bool     `json:"is_super_user"`
	Permissions   []string `json:"permission,omitzero"` // multi-valued

	// Rank is the ADR 0069 seniority (0 = owner ... 99 = none; lower is more
	// senior). nil on tokens minted before the claim existed and on
	// impersonation tokens: nil means RankNone (no authority), NEVER 0, which
	// is the owner. Pointer + omitzero keeps "absent" distinct from 0.
	Rank *int `json:"rank,omitzero"`

	// MustChangePassword is true while the Person behind the token holds a
	// temporary credential (ADR 0053 / plan D8): every authenticated route
	// except the pinned allowlist in authn answers 403 password_change_required.
	// Login and refresh mint it from the stored Person, so it survives refresh and
	// disappears with the first token minted after the password change. Tokens
	// minted before the claim existed carry no flag and read as false.
	MustChangePassword bool `json:"must_change_password,omitzero"`

	// Act carries the RFC 8693 actor claim for impersonation tokens.
	// Nil for regular tokens. Read-only at the verification layer;
	// only the Issue() path populates it.
	Act *ActClaim `json:"act,omitzero"`
}

// ActClaim is the RFC 8693 §4.1 actor claim payload — identifies the
// original operator + session under which a scoped impersonation
// token was minted. Audit middleware consumes this to populate the
// act_operator_id + act_session_id + act_reason columns on
// audit_log_entry per ADR 0045.
//
// The shape mirrors RFC 8693's nested-act recommendation: `sub` is
// the actor's identifier; LeadKart-canonical extension fields
// (session_id, reason) carry the operational context.
type ActClaim struct {
	Sub       string `json:"sub"`        // RFC 8693 §4.1 — actor's subject
	SessionID string `json:"session_id"` // impersonation session ID (links to store)
	Reason    string `json:"reason"`     // denormalised from session — at-rest queryable
}

// IsImpersonation reports whether these claims came from an
// impersonation token. Convenience over Claims.Act != nil — route-
// level middleware uses this to reject impersonation tokens at
// endpoints that don't accept them (e.g. POST /v1/platform/
// impersonation/sessions — no sub-impersonation allowed).
func (c *Claims) IsImpersonation() bool { return c != nil && c.Act != nil }

// IssueArgs are the per-call inputs to [Issuer.Issue]. Each value is
// pre-resolved by the application service; this package does no DB work.
//
// Audience overrides the default [AudienceClaim] — used by the
// impersonation flow to mint tokens with [ImpersonationAudienceClaim].
// Empty Audience falls back to AudienceClaim.
//
// TTL overrides [AccessTokenTTL] — used by the impersonation flow
// to bound the scoped token to the session lifetime. Zero TTL falls
// back to AccessTokenTTL.
//
// Act is the RFC 8693 actor claim — populated ONLY for impersonation
// tokens. Nil for regular tokens.
type IssueArgs struct {
	PersonID      string // sub claim
	TenantID      string
	TenantSlug    string
	MembershipID  string
	SecurityStamp string
	IsPlatform    bool
	IsSuperUser   bool
	Permissions   []string
	Rank          *int // ADR 0069; nil omits the claim (impersonation tokens)

	// MustChangePassword mints the must_change_password claim (login and refresh
	// pass the stored Person's flag; impersonation tokens never set it).
	MustChangePassword bool

	// Optional overrides for the impersonation path (ADR 0045).
	Audience string        // default AudienceClaim
	TTL      time.Duration // default AccessTokenTTL
	Act      *ActClaim     // default nil
}

// Issuer signs new tokens with the current key and verifies against
// the current+previous keys. Stateless — safe to share as a singleton
// once constructed (the underlying HMAC keys are immutable byte slices).
type Issuer struct {
	current  SigningKey
	previous []SigningKey // accepted for Verify only; never used to Sign
	now      func() time.Time
}

// NewIssuer wires an Issuer. previous can be nil; current.Secret MUST
// be at least 32 bytes (returns error if shorter — RFC 7518 §3.2).
//
// now is the time source — production wires time.Now; tests wire a
// fixed clock to avoid flakes around midnight UTC. Pass nil for time.Now.
func NewIssuer(current SigningKey, previous []SigningKey, now func() time.Time) (*Issuer, error) {
	if current.KeyID == "" {
		return nil, errors.New("jwt: current SigningKey.KeyID required")
	}
	if len(current.Secret) < 32 {
		return nil, fmt.Errorf("jwt: HS256 secret must be ≥32 bytes (got %d)", len(current.Secret))
	}
	if now == nil {
		now = time.Now
	}
	return &Issuer{current: current, previous: previous, now: now}, nil
}

// Issue mints a fresh access token signed with the current key.
//
// jti is generated server-side as 16 random bytes hex-encoded — every
// token gets a unique id for revocation list lookup (when JWT
// blacklist ships).
func (i *Issuer) Issue(args IssueArgs) (string, error) {
	if args.PersonID == "" {
		return "", errors.New("jwt: PersonID required")
	}
	now := i.now()
	jti, err := newJTI()
	if err != nil {
		return "", err
	}
	// Audience + TTL default to the regular API token shape; the
	// impersonation flow overrides both via IssueArgs (ADR 0045).
	aud := args.Audience
	if aud == "" {
		aud = AudienceClaim
	}
	ttl := args.TTL
	if ttl <= 0 {
		ttl = AccessTokenTTL
	}
	claims := Claims{
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    IssuerClaim,
			Audience:  jwtv5.ClaimStrings{aud},
			Subject:   args.PersonID,
			ID:        jti,
			IssuedAt:  jwtv5.NewNumericDate(now),
			NotBefore: jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(now.Add(ttl)),
		},
		TenantID:      args.TenantID,
		TenantSlug:    args.TenantSlug,
		MembershipID:  args.MembershipID,
		SecurityStamp: args.SecurityStamp,
		IsPlatform:    args.IsPlatform,
		IsSuperUser:   args.IsSuperUser,
		Permissions:   args.Permissions,
		Rank:          args.Rank,

		MustChangePassword: args.MustChangePassword,
		Act:                args.Act,
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	tok.Header["kid"] = i.current.KeyID
	signed, err := tok.SignedString(i.current.Secret)
	if err != nil {
		return "", fmt.Errorf("jwt: sign: %w", err)
	}
	return signed, nil
}

// Verify parses + validates a token, returning its claims on success
// or [ErrInvalidToken] otherwise. Tries the current signing key first,
// falls through to previous keys for the rotation grace window.
//
// Validation strictness:
//   - Algorithm MUST be HS256 (RFC 8725 §3.1 alg confusion mitigation).
//   - exp + nbf checked with ClockSkew leeway.
//   - kid header MUST match a known key id (current OR previous).
//   - iss MUST equal [IssuerClaim] (RFC 7519 §4.1.1 + RFC 8725 §3.10).
//   - aud MUST be exactly one of [AudienceClaim] OR
//     [ImpersonationAudienceClaim] (RFC 7519 §4.1.3 + RFC 8725 §3.11). The
//     accepted set is closed; any other audience, or more than one, fails.
//   - the impersonation audience and the act claim agree: aud is
//     [ImpersonationAudienceClaim] if and only if act is present. Per-route
//     reach of an impersonation token (the refusal list) is authn's, keyed on
//     the act claim this check makes trustworthy.
func (i *Issuer) Verify(token string) (*Claims, error) {
	parser := jwtv5.NewParser(
		jwtv5.WithValidMethods([]string{"HS256"}),
		jwtv5.WithLeeway(ClockSkew),
		jwtv5.WithTimeFunc(i.now),
		jwtv5.WithIssuer(IssuerClaim),
		// Audience checked post-parse — see closed-set assertion below.
		// Multi-audience needs manual validation since jwtv5 only
		// supports a single WithAudience.
	)
	claims := &Claims{}
	_, err := parser.ParseWithClaims(token, claims, func(t *jwtv5.Token) (any, error) {
		kidRaw, ok := t.Header["kid"]
		if !ok {
			return nil, errors.New("jwt: missing kid header")
		}
		kid, ok := kidRaw.(string)
		if !ok || kid == "" {
			return nil, errors.New("jwt: malformed kid header")
		}
		if kid == i.current.KeyID {
			return i.current.Secret, nil
		}
		for _, p := range i.previous {
			if p.KeyID == kid {
				return p.Secret, nil
			}
		}
		return nil, fmt.Errorf("jwt: unknown kid %q", kid)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	// Closed-set audience enforcement — RFC 8725 §3.11. Reject any
	// token whose audience isn't in our known set. Per-route policy
	// (which audience is acceptable AT THIS ENDPOINT) is enforced by
	// the calling middleware via Claims.Audience inspection.
	if !audienceAccepted(claims.Audience) {
		return nil, fmt.Errorf("%w: unexpected audience %v", ErrInvalidToken, claims.Audience)
	}
	// ONE audience rule (ADR 0045, plan D9): the impersonation audience and the
	// act claim travel together. An ordinary token is never an impersonation
	// token, and an impersonation token never passes as an ordinary one: a token
	// with exactly one of the two is malformed whichever route it reaches.
	if (claims.Audience[0] == ImpersonationAudienceClaim) != (claims.Act != nil) {
		return nil, fmt.Errorf("%w: audience %v disagrees with the act claim", ErrInvalidToken, claims.Audience)
	}
	// An act claim names the operator and the session: one without either (act: {})
	// cannot be checked against the session store, so it is malformed.
	if claims.Act != nil && (strings.TrimSpace(claims.Act.Sub) == "" || strings.TrimSpace(claims.Act.SessionID) == "") {
		return nil, fmt.Errorf("%w: act claim without its operator or session", ErrInvalidToken)
	}
	return claims, nil
}

// audienceAccepted reports whether a token's aud claim is in the
// LeadKart-accepted set (regular API tokens + impersonation tokens).
// Anything else is treated as ErrInvalidToken.
func audienceAccepted(aud jwtv5.ClaimStrings) bool {
	// Exactly one audience, from the closed set: a multi-audience token could
	// satisfy both the ordinary and the impersonation rule at once.
	return len(aud) == 1 && (aud[0] == AudienceClaim || aud[0] == ImpersonationAudienceClaim)
}

// newJTI returns a 32-char hex-encoded random id — entropy source is
// crypto/rand. Used as the jti claim for per-token revocation lookup.
func newJTI() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("jwt: rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
