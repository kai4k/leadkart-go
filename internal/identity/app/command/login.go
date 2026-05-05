package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// LoginCommand carries email + plaintext password — the password fields
// must NEVER be logged. The HTTP port is responsible for stripping these
// from request-log bodies before they reach observability.
//
// Single-Active-Membership invariant per multi-tenancy.md: a Person
// has at most ONE Active Membership at any time, so login resolves
// tenant context implicitly. No tenant slug parameter.
type LoginCommand struct {
	Email       email.Address
	Password    string
	DeviceLabel string // user-agent-derived; surfaces in /sessions UI
}

// LoginResult carries the issued tokens. The HTTP port wraps these in
// the response DTO; the access-token jwt is returned in body, the
// refresh-token plaintext goes into an HttpOnly cookie (per security.md
// "BFF cookie" canon — Web is a BFF, mobile/integrations bear-token).
type LoginResult struct {
	AccessToken         string // signed JWT
	RefreshTokenPlain   string // base64url; client stores opaquely
	AccessTokenExpiresAt time.Time
	PersonID            person.ID
	TenantID            tenant.ID
	MembershipID        membership.ID
}

// ----- Handler errors --------------------------------------------------------

// ErrInvalidCredentials is the SINGLE error returned for every
// authentication-failure path: unknown email, wrong password, no
// Active Membership, anonymised Person. Identical shape defeats user
// enumeration per OWASP "Authentication Cheat Sheet" + RFC 6749 §5.2.
var ErrInvalidCredentials = errors.New("login: invalid credentials")

// ----- Handler ---------------------------------------------------------------

// LoginHandler verifies credentials + issues a JWT + opens a refresh
// token family for the resolved (Person, Tenant) Membership. Per
// security.md "Login flow."
type LoginHandler struct {
	persons     person.Repository
	memberships membership.Repository
	families    refreshtoken.Repository
	tenants     tenant.Repository
	jwt         *jwt.Issuer
	now         func() time.Time
	refreshTTL  time.Duration

	// dummyHash flattens timing on the unknown-email branch. Computed
	// once at handler construction — Argon2id verify takes ~50-200ms,
	// so without a parallel verify on the dummy path the unknown-email
	// branch returns ~3 orders-of-magnitude faster than wrong-password,
	// which would let an attacker enumerate registered emails.
	dummyHash string
}

// NewLoginHandler wires the handler. dummyHash MUST be a valid
// argon2id PHC string of any throwaway password — Login uses it on the
// unknown-email branch to flatten timing. Production wiring computes
// it once at startup; tests may pass a precomputed constant.
func NewLoginHandler(
	persons person.Repository,
	memberships membership.Repository,
	families refreshtoken.Repository,
	tenants tenant.Repository,
	jwtIssuer *jwt.Issuer,
	now func() time.Time,
	refreshTTL time.Duration,
	dummyHash string,
) LoginHandler {
	if now == nil {
		now = time.Now
	}
	return LoginHandler{
		persons:     persons,
		memberships: memberships,
		families:    families,
		tenants:     tenants,
		jwt:         jwtIssuer,
		now:         now,
		refreshTTL:  refreshTTL,
		dummyHash:   dummyHash,
	}
}

// Handle executes the login flow per security.md "Login flow":
//
//  1. Lookup Person by email globally.
//     - null: dummy Argon2 verify → ErrInvalidCredentials.
//  2. Verify password against Person.Credential.
//     - mismatch: ErrInvalidCredentials.
//  3. Reject anonymised + inactive Persons → ErrInvalidCredentials.
//  4. Find unique Active Membership for the Person.
//     - none: ErrInvalidCredentials.
//  5. Resolve Tenant for tenant_slug claim.
//  6. Mint refresh token plaintext + hash; create RefreshTokenFamily.
//  7. Issue JWT with per-Membership claims.
//  8. Return both tokens.
//
// The unknown-email and zero-Active-Membership paths return identical
// errors AND take similar wall-clock time (the dummy verify covers it).
func (h LoginHandler) Handle(ctx context.Context, cmd LoginCommand) (LoginResult, error) {
	// 1+2. Lookup Person + verify password (unified branch).
	p, err := h.resolveAndVerify(ctx, cmd)
	if err != nil {
		return LoginResult{}, err
	}

	// 4. Find Active Membership.
	m, err := h.memberships.GetActiveForPerson(ctx, p.ID())
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, fmt.Errorf("login: lookup active membership: %w", err)
	}

	// 5. Resolve tenant for slug claim.
	tn, err := h.tenants.GetByID(ctx, m.TenantID())
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: resolve tenant: %w", err)
	}

	// 6. Mint refresh token + create family.
	mintPair, err := refreshmint.Mint()
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: mint refresh: %w", err)
	}
	family, err := refreshtoken.NewFamily(
		refreshtoken.FamilyID(ids.NewV7().String()),
		p.ID(),
		tn.ID(),
		cmd.DeviceLabel,
		mintPair.Hash,
		h.refreshTTL,
	)
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: construct family: %w", err)
	}
	if err := h.families.Add(ctx, family); err != nil {
		return LoginResult{}, fmt.Errorf("login: persist family: %w", err)
	}

	// 7. Issue JWT — embed PersonId as `sub`, per-Membership claims.
	access, err := h.jwt.Issue(jwt.IssueArgs{
		PersonID:      p.ID().String(),
		TenantID:      tn.ID().String(),
		TenantSlug:    tn.Slug().String(),
		MembershipID:  m.ID().String(),
		SecurityStamp: p.SecurityStamp().String(),
		IsPlatform:    false, // TBD: set from Membership-in-Platform-tenant lookup
		IsSuperUser:   false, // TBD: set from SuperAdmin role on the Membership
		Permissions:   nil,   // TBD: compute effective permissions from RoleAssignments
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: issue jwt: %w", err)
	}

	return LoginResult{
		AccessToken:          access,
		RefreshTokenPlain:    mintPair.Plaintext,
		AccessTokenExpiresAt: h.now().Add(jwt.AccessTokenTTL),
		PersonID:             p.ID(),
		TenantID:             tn.ID(),
		MembershipID:         m.ID(),
	}, nil
}

// resolveAndVerify covers steps 1-3: lookup Person, verify password,
// reject inactive/anonymised. Unified into one helper because the
// timing-flattening path needs to run argon2.Verify against the
// pre-computed dummyHash on the not-found branch.
func (h LoginHandler) resolveAndVerify(ctx context.Context, cmd LoginCommand) (*person.Person, error) {
	p, err := h.persons.GetByEmail(ctx, cmd.Email)
	switch {
	case errors.Is(err, person.ErrNotFound):
		// Dummy verify — same wall-clock as the wrong-password path.
		// Returned error is intentionally swallowed; this is a timing
		// flattener only.
		_ = argon2.Verify(cmd.Password, h.dummyHash)
		return nil, ErrInvalidCredentials
	case err != nil:
		return nil, fmt.Errorf("login: lookup person: %w", err)
	}

	if !p.IsActive() || p.IsAnonymised() {
		// Same dummy timing for terminal-Person paths.
		_ = argon2.Verify(cmd.Password, h.dummyHash)
		return nil, ErrInvalidCredentials
	}

	if err := argon2.Verify(cmd.Password, p.PasswordHash().String()); err != nil {
		if errors.Is(err, argon2.ErrMismatch) || errors.Is(err, argon2.ErrFormat) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("login: verify password: %w", err)
	}

	return p, nil
}

