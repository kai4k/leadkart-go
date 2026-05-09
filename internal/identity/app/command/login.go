package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
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

// AuthRouter resolves a Person + their at-most-one Active Membership
// in a single backend roundtrip. Consumer-defined per Cheney "accept
// interfaces, return structs"; the postgres-backed implementation
// lives in `internal/identity/adapters/auth_router_pg.go`.
//
// Why this is a separate concern (not a method on person.Repository
// or membership.Repository): the JOIN crosses two domain aggregates'
// query surfaces. Putting the method on either repository forces a
// cross-domain import the existing structure deliberately avoids.
//
// Returns:
//
//   - (*Person, *Membership, nil) — Person exists AND has Active Membership
//   - (*Person, nil, nil)         — Person exists but no Active Membership
//   - (nil, nil, person.ErrNotFound) — no Person matches the email
//
// The Membership returned is fully hydrated (role assignments +
// permission overrides loaded) so the resolver can compute auth
// claims without an additional roundtrip.
type AuthRouter interface {
	ResolveByEmail(ctx context.Context, e email.Address) (*person.Person, *membership.Membership, error)
}

// LoginHandler verifies credentials + issues a JWT + opens a refresh
// token family for the resolved (Person, Tenant) Membership. Per
// security.md "Login flow."
type LoginHandler struct {
	authRouter AuthRouter
	families   refreshtoken.Repository
	tenants    tenant.Repository
	resolver   *permissions.Resolver
	jwt        *jwt.Issuer
	now        func() time.Time
	refreshTTL time.Duration

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
//
// authRouter is the single-roundtrip persons+memberships JOIN
// component (consumer-defined `AuthRouter` interface, postgres impl
// in `internal/identity/adapters/auth_router_pg.go`). Replaces the
// legacy two-call pair (persons.GetByEmail + memberships.GetActiveForPerson)
// per current canon (Brandur Leach / DHH "Postgres scales further than
// you think" — single JOIN over denormalised auth tables).
func NewLoginHandler(
	authRouter AuthRouter,
	families refreshtoken.Repository,
	tenants tenant.Repository,
	resolver *permissions.Resolver,
	jwtIssuer *jwt.Issuer,
	now func() time.Time,
	refreshTTL time.Duration,
	dummyHash string,
) LoginHandler {
	if now == nil {
		now = time.Now
	}
	return LoginHandler{
		authRouter: authRouter,
		families:   families,
		tenants:    tenants,
		resolver:   resolver,
		jwt:        jwtIssuer,
		now:        now,
		refreshTTL: refreshTTL,
		dummyHash:  dummyHash,
	}
}

// Handle executes the login flow per security.md "Login flow":
//
//  1. Single-roundtrip resolve: Person + Active Membership by email.
//     - no Person:        dummy Argon2 verify → ErrInvalidCredentials.
//     - no Active Member: dummy Argon2 verify → ErrInvalidCredentials.
//  2. Verify password against Person.Credential.
//     - mismatch: ErrInvalidCredentials.
//  3. Reject anonymised + inactive Persons → ErrInvalidCredentials.
//  4. Resolve Tenant for tenant_slug claim.
//  5. Mint refresh token plaintext + hash; create RefreshTokenFamily.
//  6. Issue JWT with per-Membership claims.
//  7. Return both tokens.
//
// The unknown-email and zero-Active-Membership paths return identical
// errors AND take similar wall-clock time (the dummy verify covers
// both via the single resolveAndVerify helper).
//
// Network roundtrips: 4 in the success path (auth-routing JOIN +
// role_assignments + permission_overrides + tenant). The auth-routing
// JOIN replaces the historical persons-by-email + memberships-by-
// person-id pair — see [AuthRouter] godoc for the canonical-path
// rationale.
func (h LoginHandler) Handle(ctx context.Context, cmd LoginCommand) (LoginResult, error) {
	// 1-3. Resolve Person + Active Membership in one roundtrip;
	//      verify password + active/anonymised state with timing flattened.
	p, m, err := h.resolveAndVerify(ctx, cmd)
	if err != nil {
		return LoginResult{}, err
	}

	// 4. Resolve tenant for slug claim.
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

	// 7. Resolve effective permissions + SuperUser flag for the Membership.
	//
	//    IsPlatform stays false in v0.2 — Platform tenant ships with the
	//    v0.3 Platform module per CLAUDE.md roadmap. SuperUser doesn't
	//    require Platform-tenant membership (per multi-tenancy.md it's
	//    "Membership in Platform tenant + SuperAdmin role"); the role
	//    itself is the load-bearing flag, and IsSuperUser drives the
	//    runtime authorization short-circuit.
	//
	//    Tenant ctx binding is required: the resolver's downstream
	//    role-by-IDs query runs under TxScopeTenant, which expects
	//    `app.tenant_id` GUC set. Login is the entry point — no upstream
	//    middleware has bound it yet.
	ctx = tenancy.WithID(ctx, tenancy.ID(tn.ID().String()))
	authClaims, err := h.resolver.ResolveAuth(ctx, m)
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: resolve permissions: %w", err)
	}

	// 8. Issue JWT — embed PersonId as `sub`, per-Membership claims.
	//
	// IsPlatform is anchored to the tenant slug — matches the
	// authn.PlatformTenantSlug constant (= "platform"). Defense-in-
	// depth: a tenant impersonating "platform" via a different slug
	// can never get is_platform=true minted into its JWT. Pairs with
	// the matching slug check in authn.RequirePlatform.
	access, err := h.jwt.Issue(jwt.IssueArgs{
		PersonID:      p.ID().String(),
		TenantID:      tn.ID().String(),
		TenantSlug:    tn.Slug().String(),
		MembershipID:  m.ID().String(),
		SecurityStamp: p.SecurityStamp().String(),
		IsPlatform:    tn.Slug().String() == "platform",
		IsSuperUser:   authClaims.IsSuperUser,
		Permissions:   permissionNames(authClaims.Permissions),
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

// permissionNames flattens a [permission.Permission] slice to wire-stable
// name strings for the JWT `permission` claim. Order is stabilised by
// sorting so two logins with the same effective set produce byte-
// identical claim arrays (cache-friendly + diff-friendly in audit logs).
func permissionNames(perms []*permission.Permission) []string {
	if len(perms) == 0 {
		return nil
	}
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		if p == nil {
			continue
		}
		out = append(out, p.Name())
	}
	slices.Sort(out)
	return out
}

// resolveAndVerify covers steps 1-3: lookup (Person + Active
// Membership), verify password, reject inactive/anonymised/no-active-
// membership. Returns both aggregates so Handle can avoid the
// separate memberships.GetActiveForPerson call.
//
// Every "auth failure" branch runs argon2.Verify against the
// precomputed dummyHash before returning ErrInvalidCredentials —
// matches wrong-password wall-clock time so an attacker can't
// distinguish unknown-email / no-membership / anonymised /
// suspended from wrong-password by timing.
func (h LoginHandler) resolveAndVerify(ctx context.Context, cmd LoginCommand) (*person.Person, *membership.Membership, error) {
	p, m, err := h.authRouter.ResolveByEmail(ctx, cmd.Email)
	switch {
	case errors.Is(err, person.ErrNotFound):
		// Unknown email. Dummy verify keeps timing aligned with the
		// wrong-password path. Swallow the verify error — timing is
		// the only product here.
		_ = argon2.Verify(cmd.Password, h.dummyHash)
		return nil, nil, ErrInvalidCredentials
	case err != nil:
		return nil, nil, fmt.Errorf("login: resolve auth routing: %w", err)
	}

	if !p.IsActive() || p.IsAnonymised() {
		// Same dummy timing for terminal-Person paths.
		_ = argon2.Verify(cmd.Password, h.dummyHash)
		return nil, nil, ErrInvalidCredentials
	}

	if m == nil {
		// Person exists but has no Active Membership — same surface as
		// unknown email per OWASP "Authentication Cheat Sheet"
		// enumeration-safety. Real verify (not dummy) so even this
		// branch's timing tracks the wrong-password path exactly,
		// since the JOIN returned the actual password_hash.
		_ = argon2.Verify(cmd.Password, p.PasswordHash().String())
		return nil, nil, ErrInvalidCredentials
	}

	if err := argon2.Verify(cmd.Password, p.PasswordHash().String()); err != nil {
		if errors.Is(err, argon2.ErrMismatch) || errors.Is(err, argon2.ErrFormat) {
			return nil, nil, ErrInvalidCredentials
		}
		return nil, nil, fmt.Errorf("login: verify password: %w", err)
	}

	return p, m, nil
}

