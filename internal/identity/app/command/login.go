package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
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
//
// MustChangePassword surfaces the Person's [person.MustChangePassword]
// flag so the HTTP port can include it in the response — frontend
// redirects to the change-password screen on first login per BRD line
// 241 (admin/operator-provisioned credentials). Login itself succeeds;
// strict middleware enforcement (block all other endpoints until the
// flag clears) is a v0.3 follow-up per ADR 0053.
type LoginResult struct {
	AccessToken          string // signed JWT
	RefreshTokenPlain    string // base64url; client stores opaquely
	AccessTokenExpiresAt time.Time
	PersonID             person.ID
	TenantID             tenant.ID
	MembershipID         membership.ID
	MustChangePassword   bool
}

// ----- Handler errors --------------------------------------------------------

// ErrInvalidCredentials is the SINGLE error returned for every
// authentication-failure path: unknown email, wrong password, no
// Active Membership, anonymised Person. Identical shape defeats user
// enumeration per OWASP "Authentication Cheat Sheet" + RFC 6749 §5.2.
var ErrInvalidCredentials = errors.New("login: invalid credentials")

// ErrAccountLocked surfaces when the calling Person hit the
// [person.MaxFailedLogins] threshold within [person.LockoutWindow] and
// is currently inside the [person.LockoutDuration] cool-off. HTTP layer
// maps to 423 Locked (RFC 4918 §11.3) + Retry-After header per ADR
// 0053. Per OWASP Authentication Cheat Sheet 2025 §7.7: locked-account
// surface IS distinguishable from invalid_credentials — the user-
// facing UX needs to direct them to "wait + retry" instead of "try a
// different password". That is only safe because every attempt without a
// usable account runs a phantom lockout counter with the same thresholds
// (ADR 0068 D12): ten probes against an unknown email lock exactly like ten
// against a real one, so a 423 reveals nothing about which emails exist.
var ErrAccountLocked = errors.New("login: account locked")

// LockedUntilFromError extracts the lockout-expiry timestamp from an
// [ErrAccountLocked] error chain. Returns zero when the error is not
// a lockout. Used by the HTTP layer to compute the Retry-After header.
func LockedUntilFromError(err error) time.Time {
	var le *lockedError
	if errors.As(err, &le) {
		return le.lockedUntil
	}
	return time.Time{}
}

// lockedError wraps [ErrAccountLocked] with the LockedUntil timestamp
// so the HTTP layer can populate Retry-After without re-loading the
// Person. Internal — callers branch via errors.Is(err, ErrAccountLocked).
type lockedError struct {
	lockedUntil time.Time
}

func (e *lockedError) Error() string        { return ErrAccountLocked.Error() }
func (e *lockedError) Unwrap() error        { return ErrAccountLocked }
func (e *lockedError) Is(target error) bool { return target == ErrAccountLocked }
func newLockedError(lockedUntil time.Time) *lockedError {
	return &lockedError{lockedUntil: lockedUntil}
}

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

// PhantomLockout mirrors the per-account lockout for login attempts that
// have no usable account (unknown email, deactivated, anonymised) so 423 vs
// 401 carries no existence signal (ADR 0044 + ADR 0053; ADR 0068 D12).
//
// The implementation must keep the same thresholds and the same ATOMIC statement
// shape as person.Repository.UpdateAfterFailedLogin: one row write per failed
// attempt, decided in the database from the locked row, so that an unknown
// address and a real one do the same work and answer the same under concurrency.
// The Postgres implementation keys rows by an HMAC of the address; no address,
// plain or unsalted-hashed, is ever stored.
type PhantomLockout interface {
	// LockedUntil returns the active lockout expiry for the address, zero if none.
	// A read; nothing is written.
	LockedUntil(ctx context.Context, e email.Address, now time.Time) (time.Time, error)
	// RegisterFailure counts one failed attempt and returns the outcome read from
	// the row the same statement wrote.
	RegisterFailure(ctx context.Context, e email.Address, now time.Time) (person.LockoutOutcome, error)
}

// LoginHandler verifies credentials + issues a JWT + opens a refresh
// token family for the resolved (Person, Tenant) Membership. Per
// security.md "Login flow."
type LoginHandler struct {
	authRouter AuthRouter
	families   refreshtoken.Repository
	tenants    tenant.Repository
	persons    person.Repository
	resolver   *permissions.Resolver
	jwt        *jwt.Issuer
	now        func() time.Time
	refreshTTL time.Duration

	// newFamilyID mints the refresh-token family ID per the
	// `TestArch_HandlersInjectIDFactory` discipline. Production wires
	// `func() refreshtoken.FamilyID { return refreshtoken.FamilyID(ids.NewV7().String()) }`;
	// tests inject a deterministic counter so the family-id is pinnable.
	newFamilyID func() refreshtoken.FamilyID

	// dummyHash flattens timing on the unknown-email branch. Computed
	// once at handler construction — Argon2id verify takes ~50-200ms,
	// so without a parallel verify on the dummy path the unknown-email
	// branch returns ~3 orders-of-magnitude faster than wrong-password,
	// which would let an attacker enumerate registered emails.
	dummyHash string

	// phantom is the lockout counter for attempts with no usable account.
	phantom PhantomLockout

	// gate bounds concurrent Argon2id work (nil = unbounded, tests only);
	// production wiring passes the configured gate via [LoginHandler.WithArgonGate].
	gate *argon2.Gate

	// verify overrides the gate's verify; nil in production. Tests swap it
	// (export_test.go) to count and inspect hash operations.
	verify func(ctx context.Context, password, encoded string) error
}

// WithArgonGate returns the handler with every password verification running
// under g, so a burst of logins queues (and then answers 503) instead of
// allocating ~19 MiB each until the process is killed.
func (h LoginHandler) WithArgonGate(g *argon2.Gate) LoginHandler {
	h.gate = g
	return h
}

// verifyPassword is the one place the handler checks a password.
func (h LoginHandler) verifyPassword(ctx context.Context, password, encoded string) error {
	if h.verify != nil {
		return h.verify(ctx, password, encoded)
	}
	return h.gate.Verify(ctx, password, encoded)
}

// dummyVerify spends one password verification against the throwaway hash so a
// branch with no real hash costs what a wrong password costs. Only a refusal to
// run it at all (the gate is saturated, the request is gone) is an error: every
// branch must answer that the same way, or the branch would be visible.
func (h LoginHandler) dummyVerify(ctx context.Context, password string) error {
	return verifyRefusal(ctx, h.verifyPassword(ctx, password, h.dummyHash))
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
//
// persons is the [person.Repository] used by the Wave 9.2 lockout flow
// to persist the FailedLoginCount + LockedUntil counters via the
// dedicated UpdateLockoutState hot-path (cheaper than UpdateByID for
// the high-frequency wrong-password branch).
func NewLoginHandler(
	authRouter AuthRouter,
	families refreshtoken.Repository,
	tenants tenant.Repository,
	persons person.Repository,
	resolver *permissions.Resolver,
	jwtIssuer *jwt.Issuer,
	now func() time.Time,
	refreshTTL time.Duration,
	dummyHash string,
	newFamilyID func() refreshtoken.FamilyID,
	phantom PhantomLockout,
) LoginHandler {
	if newFamilyID == nil {
		panic("command: NewLoginHandler newFamilyID required")
	}
	if phantom == nil {
		panic("command: NewLoginHandler phantom lockout required")
	}
	if now == nil {
		now = time.Now
	}
	return LoginHandler{
		authRouter:  authRouter,
		families:    families,
		tenants:     tenants,
		persons:     persons,
		resolver:    resolver,
		jwt:         jwtIssuer,
		now:         now,
		refreshTTL:  refreshTTL,
		dummyHash:   dummyHash,
		newFamilyID: newFamilyID,
		phantom:     phantom,
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
		h.newFamilyID(),
		p.ID(),
		tn.ID(),
		cmd.DeviceLabel,
		mintPair.Hash,
		h.refreshTTL,
		h.now(),
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
		IsPlatform:    tn.Slug().String() == tenant.PlatformSlug,
		IsSuperUser:   authClaims.IsSuperUser,
		Permissions:   permissionNames(authClaims.Permissions),
		Rank:          rankClaim(authClaims.Rank),
		// The flag rides in the token (plan D8): authn answers 403
		// password_change_required on every non-allowlisted route until it clears.
		MustChangePassword: p.MustChangePassword(),
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
		MustChangePassword:   p.MustChangePassword(),
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

// resolveAndVerify covers steps 1-4: lookup (Person + Active
// Membership), check lockout BEFORE password verify (NIST 800-63B
// §5.2.2 + ADR 0053 — avoid timing leak), verify password, reject
// inactive/anonymised/no-active-membership. On wrong password the failure is
// counted by ONE atomic statement and the answer (401 or the threshold-crossing
// 423) is read from the row it wrote; on correct, the counter is cleared.
//
// Every failure branch does the same work: one password verify (against the real
// hash for a wrong password, against dummyHash for an unknown email, a terminal
// Person and a locked account) and, unless the account is already locked, one
// row write (the person's counter, or the phantom counter for an address with no
// usable account). Both counters are the same atomic statement shape, so the
// branches cannot be told apart by count, status, body, header or concurrency
// behaviour. The clock is read once, AFTER the verify, on every branch, so
// Retry-After and the lock length match to the second.
func (h LoginHandler) resolveAndVerify(ctx context.Context, cmd LoginCommand) (*person.Person, *membership.Membership, error) {
	p, m, err := h.authRouter.ResolveByEmail(ctx, cmd.Email)
	switch {
	case errors.Is(err, person.ErrNotFound):
		return nil, nil, h.failWithoutAccount(ctx, cmd)
	case err != nil:
		return nil, nil, fmt.Errorf("login: resolve auth routing: %w", err)
	}

	// Terminal Persons (deactivated, anonymised) are "no account" for login:
	// they answer exactly like an unknown email, phantom counter included, and
	// this check runs BEFORE the real lockout gate so a Person who was locked
	// and then deactivated cannot answer 423 where an unknown email answers 401.
	// Their real counter is never bumped — these are admin-controlled states,
	// not credential attempts the user can recover from.
	if !p.IsActive() || p.IsAnonymised() {
		return nil, nil, h.failWithoutAccount(ctx, cmd)
	}

	// Lockout gate — the real password is NEVER checked while locked (NIST
	// 800-63B §5.2.2 + ADR 0053), but one dummy verify still runs so this branch
	// costs what a wrong password costs: otherwise a fast 423 would tell an
	// attacker the account exists and is under attack. The answer is the same
	// 423 + Retry-After a threshold-crossing wrong password gets.
	if p.IsLocked(h.now()) {
		if err := h.dummyVerify(ctx, cmd.Password); err != nil {
			return nil, nil, err
		}
		return nil, nil, newLockedError(p.LockedUntil())
	}

	verifyErr := h.verifyPassword(ctx, cmd.Password, p.PasswordHash().String())

	if m == nil {
		// Person exists but has no Active Membership — same surface as
		// unknown email per OWASP "Authentication Cheat Sheet"
		// enumeration-safety. Real verify (not dummy) so this branch's
		// timing tracks the wrong-password path exactly (the JOIN returned
		// the real hash). Count only a WRONG password as a failed attempt:
		// a correct password proves it's not a brute-force probe, just a
		// user with no active membership (off-boarded / mid-re-onboarding),
		// and must not accrue lockout. The threshold-crossing attempt answers
		// 423 like the wrong-password branch, so the two cannot be told apart
		// by the tenth attempt.
		if verifyErr != nil {
			if refusal := verifyRefusal(ctx, verifyErr); refusal != nil {
				return nil, nil, refusal
			}
			return nil, nil, h.failedAttempt(ctx, p)
		}
		return nil, nil, ErrInvalidCredentials
	}

	if verifyErr != nil {
		if errors.Is(verifyErr, argon2.ErrMismatch) || errors.Is(verifyErr, argon2.ErrFormat) {
			return nil, nil, h.failedAttempt(ctx, p)
		}
		if refusal := verifyRefusal(ctx, verifyErr); refusal != nil {
			return nil, nil, refusal
		}
		return nil, nil, fmt.Errorf("login: verify password: %w", verifyErr)
	}

	// Successful credential verify — clear counter + lockout.
	h.registerSuccessfulLoginBestEffort(ctx, p)

	return p, m, nil
}

// verifyRefusal returns the error to answer when the verification could not run
// at all (gate saturated, request cancelled), nil when it ran (whatever its
// result).
func verifyRefusal(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, argon2.ErrBusy):
		return fmt.Errorf("login: %w", err)
	case ctx.Err() != nil && err != nil:
		return fmt.Errorf("login: %w", ctx.Err())
	}
	return nil
}

// failedAttempt counts one failed attempt against a real account and returns the
// error to answer: the 423 when THIS attempt's own atomic statement left the
// account locked (it crossed the threshold, or a concurrent attempt had), the
// plain invalid-credentials otherwise. now is read here, after the verify.
//
// Best-effort like the rest of the lockout (ADR 0053): a storage error is
// swallowed and answers 401, because failing the login on a counter error would
// hand an attacker a way to turn database trouble into 500s.
func (h LoginHandler) failedAttempt(ctx context.Context, p *person.Person) error {
	now := h.now()
	out, err := h.persons.UpdateAfterFailedLogin(ctx, p.ID(), now)
	if err == nil && out.LockedAt(now) {
		return newLockedError(out.LockedUntil)
	}
	return ErrInvalidCredentials
}

// failWithoutAccount is the single failure surface for login attempts with no
// usable account (unknown email, deactivated, anonymised). It does exactly what
// a real account's wrong-password branch does: check the lock (a read), run one
// dummy verify, and — unless already locked — register one failed attempt with
// the same atomic statement shape, so the tenth attempt in the window answers 423
// exactly where a real account would and 423 reveals nothing about whether the
// email exists.
func (h LoginHandler) failWithoutAccount(ctx context.Context, cmd LoginCommand) error {
	until, lockErr := h.phantom.LockedUntil(ctx, cmd.Email, h.now())
	if err := h.dummyVerify(ctx, cmd.Password); err != nil {
		return err
	}
	if lockErr == nil && !until.IsZero() {
		return newLockedError(until)
	}
	now := h.now()
	out, err := h.phantom.RegisterFailure(ctx, cmd.Email, now)
	if err == nil && out.LockedAt(now) {
		return newLockedError(out.LockedUntil)
	}
	return ErrInvalidCredentials
}

// registerSuccessfulLoginBestEffort applies [Person.RegisterSuccessfulLogin]
// + persists via [person.Repository.UpdateLockoutState]. Best-effort like the
// failed-login path.
func (h LoginHandler) registerSuccessfulLoginBestEffort(ctx context.Context, p *person.Person) {
	p.RegisterSuccessfulLogin(h.now())
	_ = h.persons.UpdateLockoutState(ctx, p)
}
