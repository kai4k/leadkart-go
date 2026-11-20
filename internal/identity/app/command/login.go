package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
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

// ErrTenantSuspended is returned by login when the password was verified and the
// person's tenant is suspended: the members of a suspended company cannot sign in.
// It is only ever returned AFTER the password check, so it tells someone who does
// not know the password nothing (a wrong password or an unknown email still answers
// [ErrInvalidCredentials]). Refresh wraps it together with [ErrRefreshRejected].
var ErrTenantSuspended = errors.New("tenant suspended")

// ErrTenantNotActive is returned by login when the password was verified and the
// person's company is neither active nor suspended nor deleted: it is pending or
// marked for deletion. Owner ruling 2026-11-21: "If a Company is not Active no one
// can login from that org." Like [ErrTenantSuspended] it is only ever returned AFTER
// the password check. Refresh wraps it together with [ErrRefreshRejected].
var ErrTenantNotActive = errors.New("tenant not active")

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
// The Membership returned is the row only: its role assignments and permission
// overrides are NOT loaded. Every branch of a login that fails must cost the same
// queries, so the two extra reads happen only once the password verified
// ([AuthRouter.LoadMembership]); the row is enough to know there is an active
// membership and which tenant it is in.
type AuthRouter interface {
	ResolveByEmail(ctx context.Context, e email.Address) (*person.Person, *membership.Membership, error)
	// LoadMembership returns the membership fully hydrated (role assignments and
	// permission overrides loaded) so the resolver can compute auth claims.
	LoadMembership(ctx context.Context, id membership.ID) (*membership.Membership, error)
	// CheckSessionTarget re-reads, in the ambient session transaction when there is
	// one, the facts a new session depends on (the membership's status, owner and
	// tenant, and the tenant's status) as they are NOW. A plain read: it takes no
	// row lock, so it adds no edge to the lock order. [membership.ErrNotFound] when
	// the membership is gone.
	CheckSessionTarget(ctx context.Context, id membership.ID) (SessionTarget, error)
}

// SessionTarget is what [AuthRouter.CheckSessionTarget] re-reads inside the session
// transaction: the facts a new session depends on, as they are NOW.
type SessionTarget struct {
	PersonID         person.ID
	TenantID         tenant.ID
	MembershipActive bool
	TenantStatus     tenant.Status
}

// PhantomFailureCounter mirrors the per-account lockout for login attempts that
// have no usable account (unknown email, deactivated, anonymised) so 423 vs
// 401 carries no existence signal (ADR 0044 + ADR 0053; ADR 0068 D12).
//
// The implementation must keep the same thresholds and the same ATOMIC statement
// shape as person.Repository.UpdateAfterFailedLogin: one row write per attempt,
// decided in the database from the locked row, so that an unknown address and a
// real one do the same work and answer the same under concurrency. There is
// deliberately no separate read of the lock: the attempt is registered first and
// the answer comes from the row the same statement wrote. The Postgres
// implementation keys rows by an HMAC of the address; no address, plain or
// unsalted-hashed, is ever stored.
type PhantomFailureCounter interface {
	// RegisterFailure counts one attempt and returns the outcome read from the row
	// the same statement wrote.
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
	uow        pg.UnitOfWork
	resolver   *permissions.Resolver
	jwt        *jwt.Issuer
	now        func() time.Time
	// refreshTTL is Refresh.AbsoluteTTL (the family's absolute lifetime).
	refreshTTL time.Duration
	// idleTTL is Refresh.SlidingTTL: the lifetime of ONE refresh token, so a session
	// not refreshed for that long ends. Never above refreshTTL ([sessionTTLs]).
	idleTTL time.Duration

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
	phantom PhantomFailureCounter

	// gate bounds concurrent Argon2id work (nil = unbounded, tests only);
	// production wiring passes the configured gate via [LoginHandler.WithArgonGate].
	gate *argon2.Gate

	// verify overrides the gate's verify; nil in production. Tests swap it
	// (export_test.go) to count and inspect hash operations.
	verify func(ctx context.Context, password, encoded string) error

	// acquire overrides the gate's slot acquisition; nil in production. Tests swap
	// it (export_test.go) to refuse an attempt before it is registered.
	acquire func(ctx context.Context) (release func(), err error)
}

// WithArgonGate returns the handler with every password verification running
// under g, so a burst of logins queues (and then answers 503) instead of
// allocating ~19 MiB each until the process is killed.
func (h LoginHandler) WithArgonGate(g *argon2.Gate) LoginHandler {
	h.gate = g
	return h
}

// acquireSlot takes the attempt's gate slot. It is taken BEFORE the attempt is
// registered and held through its one verify: an attempt the gate refuses (busy,
// cancelled) never ran and writes nothing, so saturating the gate cannot be used
// to lock an account or to burn the phantom table, and the refusal is the same
// answer on every branch.
func (h LoginHandler) acquireSlot(ctx context.Context) (func(), error) {
	if h.acquire != nil {
		return h.acquire(ctx)
	}
	return h.gate.Acquire(ctx)
}

// verifyPassword is the one place the handler checks a password; the caller holds
// the attempt's gate slot.
func (h LoginHandler) verifyPassword(ctx context.Context, password, encoded string) error {
	if h.verify != nil {
		return h.verify(ctx, password, encoded)
	}
	return h.gate.VerifyHeld(password, encoded)
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
// persons is the [person.Repository] used by the lockout flow (the atomic
// register / clear statements) and, under uow, by the session insert's check that
// the verified credential is still the stored one.
func NewLoginHandler(
	authRouter AuthRouter,
	families refreshtoken.Repository,
	tenants tenant.Repository,
	persons person.Repository,
	resolver *permissions.Resolver,
	jwtIssuer *jwt.Issuer,
	now func() time.Time,
	refreshTTL time.Duration,
	idleTTL time.Duration,
	dummyHash string,
	newFamilyID func() refreshtoken.FamilyID,
	phantom PhantomFailureCounter,
	uow pg.UnitOfWork,
) LoginHandler {
	if newFamilyID == nil {
		panic("command: NewLoginHandler newFamilyID required")
	}
	if phantom == nil {
		panic("command: NewLoginHandler phantom lockout required")
	}
	if uow == nil {
		panic("command: NewLoginHandler unit of work required")
	}
	if now == nil {
		now = time.Now
	}
	return LoginHandler{
		authRouter:  authRouter,
		families:    families,
		tenants:     tenants,
		persons:     persons,
		uow:         uow,
		resolver:    resolver,
		jwt:         jwtIssuer,
		now:         now,
		refreshTTL:  refreshTTL,
		idleTTL:     idleTTL,
		dummyHash:   dummyHash,
		newFamilyID: newFamilyID,
		phantom:     phantom,
	}
}

// Handle executes the login flow per security.md "Login flow":
//
//  1. Single-roundtrip resolve: Person + Active Membership row by email.
//     - no Person:        register the attempt, dummy verify → ErrInvalidCredentials.
//     - no Active Member: register the attempt, real verify → ErrInvalidCredentials.
//  2. Register the attempt atomically BEFORE the verify; locked → dummy verify, 423.
//  3. Verify the password against the stored hash; a correct one clears the
//     attempt's own count without ever clearing a lock.
//  4. Reject anonymised + inactive Persons → ErrInvalidCredentials.
//  5. Load the full membership + the Tenant (tenant_slug claim).
//  6. In ONE transaction: re-read the person under a SHARE lock, refuse when the
//     credential is no longer the verified one or the tenant's members may not sign
//     in (suspended: [ErrTenantSuspended]; pending or marked for deletion:
//     [ErrTenantNotActive]; deleted: [ErrInvalidCredentials]), insert
//     the refresh family.
//  7. Issue JWT with per-Membership claims and the stamp read under that lock.
//
// The failure branches take the same steps in the same order and cost the same
// queries; everything that differs between accounts (the membership hydration,
// the permission resolution, the tenant read) happens only after a successful
// verify.
func (h LoginHandler) Handle(ctx context.Context, cmd LoginCommand) (LoginResult, error) {
	v, err := h.resolveAndVerify(ctx, cmd)
	if err != nil {
		return LoginResult{}, err
	}
	p := v.person

	// 5. Hydrate the membership (roles + overrides) and resolve the tenant.
	m, err := h.authRouter.LoadMembership(ctx, v.membershipID)
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: load membership: %w", err)
	}
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
		idleLifetime(h.idleTTL, h.refreshTTL),
		h.now(),
	)
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: construct family: %w", err)
	}
	stamp, mustChange, err := h.openSession(ctx, p.ID(), v.verifiedHash, m.ID(), family)
	if err != nil {
		return LoginResult{}, err
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
	//
	// The stamp and the must-change flag are the ones read under the session
	// insert's SHARE lock, so the token matches the credential the family belongs to.
	access, err := h.jwt.Issue(jwt.IssueArgs{
		PersonID:      p.ID().String(),
		TenantID:      tn.ID().String(),
		TenantSlug:    tn.Slug().String(),
		MembershipID:  m.ID().String(),
		SecurityStamp: stamp,
		IsPlatform:    tn.Slug().String() == tenant.PlatformSlug,
		IsSuperUser:   authClaims.IsSuperUser,
		Permissions:   permissionNames(authClaims.Permissions),
		Rank:          rankClaim(authClaims.Rank),
		// The flag rides in the token (plan D8): authn answers 403
		// password_change_required on every non-allowlisted route until it clears.
		MustChangePassword: mustChange,
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
		MustChangePassword:   mustChange,
	}, nil
}

// openSession inserts the family in a transaction that first re-reads the person
// under a SHARE lock (fix round 12, item 1). The login verified the password some
// milliseconds (and up to the gate's queue time) ago, in no transaction; a password
// change or reset that committed since has revoked the families it could see, and
// this one was not yet visible to it. Under the share lock the read waits for a
// change that is committing and sees its result: when the stored hash is no longer
// the one that was verified, or the person is no longer active, the login is
// refused exactly like a wrong password and no family row exists. A change that
// starts after this read waits for this transaction to commit, then lists and
// revokes the family. Either way no session survives a credential change.
//
// After the person read, in the same transaction, the membership and the tenant are
// re-read ([AuthRouter.CheckSessionTarget]): a membership deactivated, moved to
// another tenant or removed, or a tenant deleted, since the login resolved them
// yields no session (the same 401 as a wrong password). That read takes NO lock (a
// plain SELECT), so the lock order is unchanged: authority commands take tenant
// advisory → membership → person; this transaction takes person (share) then the
// NEW family row and only READS the membership, so there is no membership-after-
// person edge. A deactivation that commits after this read is caught by its own
// family revocation (it lists every family of the person once it commits) or, at
// the latest, by the next refresh, which re-checks the membership.
//
// Lock order here: person (share) → the NEW family row. See sql/persons.sql.
// Returns the security stamp and must-change flag read under that lock.
func (h LoginHandler) openSession(ctx context.Context, id person.ID, verified person.PasswordHash, membershipID membership.ID, family *refreshtoken.Family) (stamp string, mustChange bool, err error) {
	err = h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		cur, err := h.persons.GetByIDForShare(ctx, id)
		if err != nil {
			if errors.Is(err, person.ErrNotFound) {
				return ErrInvalidCredentials
			}
			return fmt.Errorf("login: re-read person: %w", err)
		}
		if cur.PasswordHash() != verified || !cur.MaySignIn() {
			return ErrInvalidCredentials
		}
		target, err := h.authRouter.CheckSessionTarget(ctx, membershipID)
		if err != nil {
			if errors.Is(err, membership.ErrNotFound) {
				return ErrInvalidCredentials
			}
			return fmt.Errorf("login: re-read membership: %w", err)
		}
		if !target.MembershipActive || target.PersonID != id || target.TenantID != family.TenantID() {
			return ErrInvalidCredentials
		}
		// The password was verified (and is still the stored one, above), so naming
		// the status reveals nothing to someone who does not know it. A deleted (or
		// otherwise unusable) tenant stays the ordinary refusal.
		if !target.TenantStatus.MembersMaySignIn() {
			switch target.TenantStatus {
			case tenant.StatusSuspended:
				return ErrTenantSuspended
			case tenant.StatusPending, tenant.StatusPendingDeletion:
				return ErrTenantNotActive
			default:
				return ErrInvalidCredentials
			}
		}
		if err := h.families.Add(ctx, family); err != nil {
			return fmt.Errorf("login: persist family: %w", err)
		}
		stamp, mustChange = cur.SecurityStamp().String(), cur.MustChangePassword()
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return "", false, ErrInvalidCredentials
		}
		return "", false, err
	}
	return stamp, mustChange, nil
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

// verifiedLogin is what a successful verify hands to the session step.
type verifiedLogin struct {
	person       *person.Person
	membershipID membership.ID
	// verifiedHash is the stored hash the password was checked against, captured
	// BEFORE the verify: the session insert refuses when the stored hash moved.
	verifiedHash person.PasswordHash
}

// boundedPassword returns the password to hash and whether it was over the size
// bound. Every password was bounded when it was set (tenant.MaxPasswordBytes), so an
// over-long one cannot be the stored one: it is never passed to the real hash, only
// its first MaxPasswordBytes bytes go to the dummy hash, and the attempt is
// answered as a wrong password on every branch.
func boundedPassword(password string) (string, bool) {
	if len(password) > tenant.MaxPasswordBytes {
		return password[:tenant.MaxPasswordBytes], true
	}
	return password, false
}

// resolveAndVerify covers the lookup, the lockout, the password check and the
// account-state gates.
//
// Every attempt is REGISTERED before its password is checked, by one atomic
// statement (ADR 0068 D12): the answer "locked" is read from the row that statement
// wrote, so concurrent attempts cannot all see "unlocked" and verify against the
// real hash (about 90 guesses per lock cycle instead of 10), and a correct guess
// that arrives inside the batch cannot log in while the account is locked. When the
// row says locked by an EARLIER attempt the real password is NEVER checked (NIST
// 800-63B §5.2.2 + ADR 0053) but one dummy verify still runs so the branch costs
// what a wrong password costs. A correct password then clears the attempt's own count with a conditional
// statement that refuses to clear a lock (a concurrent failure may have set one
// meanwhile): zero rows cleared answers 423 and opens no session. The visible
// effect of increment-then-reset on a correct login is none; its cost is two row
// writes instead of one, which is no oracle because success is distinguishable
// anyway.
//
// Every failure branch does the same work: one atomic row write (the person's
// counter, or the phantom counter for an address with no usable account), then one
// password verify (the real hash for a wrong password, the dummy hash for an
// unknown email, a terminal Person, a locked account or an over-long password).
// The branches cannot be told apart by count, status, body, header or concurrency
// behaviour. The clock is read once, at the registration, on every branch, so
// Retry-After and the lock length match to the second. A window allows at most
// MaxFailedLogins real verifies: the attempts registered before the lock, the last
// of which is the one that locks.
func (h LoginHandler) resolveAndVerify(ctx context.Context, cmd LoginCommand) (verifiedLogin, error) {
	p, m, err := h.authRouter.ResolveByEmail(ctx, cmd.Email)
	unknown := errors.Is(err, person.ErrNotFound)
	if err != nil && !unknown {
		return verifiedLogin{}, fmt.Errorf("login: resolve auth routing: %w", err)
	}

	// The attempt's slot, before anything is written (see acquireSlot).
	release, err := h.acquireSlot(ctx)
	if err != nil {
		return verifiedLogin{}, verifyRefusalOrWrap(ctx, err)
	}
	defer release()

	if unknown {
		return verifiedLogin{}, h.failWithoutAccount(ctx, cmd)
	}

	// Terminal Persons (deactivated, anonymised, globally suspended) are "no account" for login:
	// they answer exactly like an unknown email, phantom counter included, and
	// this check runs BEFORE the real lockout gate so a Person who was locked
	// and then deactivated cannot answer 423 where an unknown email answers 401.
	// Their real counter is never bumped — these are admin-controlled states,
	// not credential attempts the user can recover from.
	if !p.MaySignIn() {
		return verifiedLogin{}, h.failWithoutAccount(ctx, cmd)
	}

	return h.attemptRealAccount(ctx, cmd, p, m)
}

// attemptRealAccount is the attempt against a usable account: register, lock gate,
// the one verify, the success clear. See [LoginHandler.resolveAndVerify].
func (h LoginHandler) attemptRealAccount(ctx context.Context, cmd LoginCommand, p *person.Person, m *membership.Membership) (verifiedLogin, error) {
	password, oversize := boundedPassword(cmd.Password)

	// Register the attempt first; the lock decision is read from the row it wrote.
	now := h.now()
	out, err := h.persons.UpdateAfterFailedLogin(ctx, p.ID(), now)
	switch {
	case errors.Is(err, person.ErrNotFound):
		// Deleted between the resolve and now: the same answer as an unknown email.
		if err := h.dummyVerify(ctx, password); err != nil {
			return verifiedLogin{}, err
		}
		return verifiedLogin{}, ErrInvalidCredentials
	case err != nil:
		return verifiedLogin{}, fmt.Errorf("login: register attempt: %w", err)
	}
	if out.LockedAt(now) && !out.JustLocked {
		// Locked by an earlier attempt: the real password is not checked.
		if err := h.dummyVerify(ctx, password); err != nil {
			return verifiedLogin{}, err
		}
		return verifiedLogin{}, newLockedError(out.LockedUntil)
	}
	// This attempt may be the one that crossed the threshold (out.JustLocked): its
	// password is still checked, as before, so ten wrong passwords lock and nine
	// wrong plus the right one signs in. It answers 423 when wrong, and when right
	// it clears only the lock it set itself.
	var ownLock time.Time
	if out.JustLocked {
		ownLock = out.LockedUntil
	}

	verifiedHash := p.PasswordHash()
	var verifyErr error
	if oversize {
		// Never hash an over-long password against the real hash: one dummy verify
		// stands in, and the result is the mismatch a wrong password gives.
		if err := h.dummyVerify(ctx, password); err != nil {
			return verifiedLogin{}, err
		}
		verifyErr = argon2.ErrMismatch
	} else {
		verifyErr = h.verifyPassword(ctx, password, verifiedHash.String())
	}
	if verifyErr != nil {
		if errors.Is(verifyErr, argon2.ErrMismatch) || errors.Is(verifyErr, argon2.ErrFormat) {
			if out.JustLocked {
				return verifiedLogin{}, newLockedError(out.LockedUntil) // the threshold-crossing attempt
			}
			return verifiedLogin{}, ErrInvalidCredentials // already counted by the registration
		}
		if refusal := verifyRefusal(ctx, verifyErr); refusal != nil {
			return verifiedLogin{}, refusal
		}
		return verifiedLogin{}, fmt.Errorf("login: verify password: %w", verifyErr)
	}

	// The password is right: clear the attempt's own count, never a lock. A person
	// with no active membership (off-boarded, mid-re-onboarding) proves it is not a
	// brute-force probe and must not accrue lockout, so this runs for it as well.
	cleared, err := h.persons.UpdateAfterSuccessfulLogin(ctx, p.ID(), h.now(), ownLock)
	if err != nil {
		return verifiedLogin{}, fmt.Errorf("login: clear attempt: %w", err)
	}
	if !cleared.Cleared {
		return verifiedLogin{}, newLockedError(cleared.LockedUntil)
	}
	if m == nil {
		// Same surface as unknown email per OWASP "Authentication Cheat Sheet"
		// enumeration-safety; the real verify above kept this branch's timing on the
		// wrong-password path.
		return verifiedLogin{}, ErrInvalidCredentials
	}
	return verifiedLogin{person: p, membershipID: m.ID(), verifiedHash: verifiedHash}, nil
}

// verifyRefusalOrWrap is the answer for a slot that could not be taken: the
// refusal itself (busy, cancelled), identically on every branch.
func verifyRefusalOrWrap(ctx context.Context, err error) error {
	if refusal := verifyRefusal(ctx, err); refusal != nil {
		return refusal
	}
	return fmt.Errorf("login: %w", err)
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

// failWithoutAccount is the single failure surface for login attempts with no
// usable account (unknown email, deactivated, anonymised). It does exactly what
// a real account's wrong-password branch does, in the same order: register one
// attempt with the same atomic statement shape (the clock read once, here), then
// run one dummy verify, then answer 423 where the row says locked and 401 otherwise,
// so the tenth attempt in the window answers 423 exactly where a real account would
// and 423 reveals nothing about whether the email exists.
func (h LoginHandler) failWithoutAccount(ctx context.Context, cmd LoginCommand) error {
	password, _ := boundedPassword(cmd.Password)
	now := h.now()
	out, err := h.phantom.RegisterFailure(ctx, cmd.Email, now)
	if err != nil {
		return fmt.Errorf("login: register attempt: %w", err)
	}
	if err := h.dummyVerify(ctx, password); err != nil {
		return err
	}
	if out.LockedAt(now) {
		return newLockedError(out.LockedUntil)
	}
	return ErrInvalidCredentials
}
