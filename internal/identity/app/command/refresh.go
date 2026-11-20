package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RefreshCommand carries the plaintext refresh-token presented by the
// client. The handler hashes it server-side and looks up the family by
// hash — plaintext NEVER reaches a query.
//
// No PersonID, no TenantID — the family carries them. Per security.md
// "Refresh token — family pattern": tokens self-identify by hash lookup.
type RefreshCommand struct {
	RefreshTokenPlain string
}

// RefreshResult mirrors LoginResult — same access+refresh pair shape.
type RefreshResult struct {
	AccessToken          string
	RefreshTokenPlain    string
	AccessTokenExpiresAt time.Time
	PersonID             person.ID
	TenantID             tenant.ID
	MembershipID         membership.ID
	// MustChangePassword mirrors the claim minted into AccessToken.
	MustChangePassword bool
}

// ----- Handler errors --------------------------------------------------------

// ErrRefreshRejected is the SINGLE error for every refresh-failure
// path: unknown hash, revoked family, expired token, reuse detected.
// Identical shape defeats family-id enumeration. Reuse-detected
// causes the family to be revoked as a side effect (RFC 9700 §4.13).
var ErrRefreshRejected = errors.New("refresh: rejected")

// ErrSessionExpired accompanies [ErrRefreshRejected] (both match errors.Is) when
// the refused token belongs to a session that reached its absolute lifetime
// (Refresh.AbsoluteTTL from the login): the BFF maps it to "your session ended,
// sign in again" instead of a generic failure. It reveals nothing a holder of the
// token does not know, and it is the only refusal the answer names.
var ErrSessionExpired = errors.New("refresh: session reached its absolute lifetime")

// ----- Handler ---------------------------------------------------------------

// RefreshHandler rotates the refresh-token chain + reissues a JWT.
// Per RFC 9700 §4.13 + security.md "Refresh token rotation".
type RefreshHandler struct {
	families    refreshtoken.Repository
	persons     person.Repository
	memberships membership.Repository
	tenants     tenant.Repository
	resolver    *permissions.Resolver
	jwt         *jwt.Issuer
	now         func() time.Time
	// refreshTTL is Refresh.AbsoluteTTL: the lifetime of one refresh token AND the
	// absolute lifetime of a family counted from its creation (a rotation never
	// extends a session past it, see Family.RotateWithGrace).
	refreshTTL time.Duration
	// graceWindow is how long a just-rotated token may be presented again and
	// receive the same successor (zero: strict rotation, ADR 0068 D10).
	graceWindow time.Duration
	// successorKey keys the deterministic successor derivation
	// (refreshmint.DeriveSuccessorKey of the JWT signing secret).
	successorKey []byte
	// bindingKey keys the HMAC that binds a consumed token to its client
	// (refreshmint.DeriveClientBindingKey of successorKey).
	bindingKey []byte

	// log and graceReplays are the telemetry of a grace replay: a structured line
	// and the counter leadkart.identity.refresh_grace_replays{outcome}. log is nil
	// until [RefreshHandler.WithLogger] (tests); the counter is a no-op without a
	// meter provider.
	log          *slog.Logger
	graceReplays metric.Int64Counter
}

// WithLogger returns the handler logging every grace replay (a WARN for a refused
// one). The line carries the family id and the outcome, never a token, a client
// address or a binding.
func (h RefreshHandler) WithLogger(log *slog.Logger) RefreshHandler {
	h.log = log
	return h
}

// Grace-replay outcomes: the values of the `outcome` attribute of
// leadkart.identity.refresh_grace_replays and of the log line.
const (
	graceOutcomeSameClient         = "same_client"
	graceOutcomeRefusedOtherClient = "refused_other_client"
	graceOutcomeRefusedLimit       = "refused_limit"
)

// NewRefreshHandler wires the handler.
func NewRefreshHandler(
	families refreshtoken.Repository,
	persons person.Repository,
	memberships membership.Repository,
	tenants tenant.Repository,
	resolver *permissions.Resolver,
	jwtIssuer *jwt.Issuer,
	now func() time.Time,
	refreshTTL time.Duration,
	graceWindow time.Duration,
	successorKey []byte,
) RefreshHandler {
	if len(successorKey) < refreshmint.SuccessorKeyLen {
		panic("command: NewRefreshHandler requires a successor key of at least 32 bytes")
	}
	if now == nil {
		now = time.Now
	}
	bindingKey, err := refreshmint.DeriveClientBindingKey(successorKey)
	if err != nil {
		panic(fmt.Errorf("command: NewRefreshHandler client binding key: %w", err))
	}
	counter, cerr := otel.Meter("leadkart/identity").Int64Counter("leadkart.identity.refresh_grace_replays",
		metric.WithDescription("Presentations of a just-rotated refresh token inside its grace window, by outcome."))
	if cerr != nil {
		counter = nil
	}
	return RefreshHandler{
		bindingKey:   bindingKey,
		graceReplays: counter,
		families:     families,
		persons:      persons,
		memberships:  memberships,
		tenants:      tenants,
		resolver:     resolver,
		jwt:          jwtIssuer,
		now:          now,
		refreshTTL:   refreshTTL,

		graceWindow:  graceWindow,
		successorKey: successorKey,
	}
}

// Handle executes the rotation flow:
//
//  1. Hash the presented plaintext.
//  2. Look up family by hash → ErrRefreshRejected if not found.
//  3. Derive the successor ⟨plaintext, hash⟩ from the presented token and the
//     family id (deterministic: a re-presentation inside the grace window
//     derives the SAME successor).
//  4. UpdateByID closure (family row locked FOR NO KEY UPDATE) runs
//     Family.RotateWithGrace(presentedHash, successorHash, grace).
//     - A grace replay writes nothing and carries on with the same successor.
//     - On reuse-detection: aggregate emits RevokedEvent; the closure
//     returns shouldPersist=true alongside ErrReuseDetected so the
//     revocation is committed. Caller receives ErrRefreshRejected.
//     - On any other rotate error: returns ErrRefreshRejected.
//  5. Resolve Person + Tenant + Membership for the new JWT claims.
//  6. Issue JWT.
func (h RefreshHandler) Handle(ctx context.Context, cmd RefreshCommand) (RefreshResult, error) {
	presentedHash, err := refreshtoken.NewTokenHash(refreshmint.HashOf(cmd.RefreshTokenPlain))
	if err != nil {
		return RefreshResult{}, ErrRefreshRejected
	}

	// 2. Look up family by token hash.
	family, err := h.families.GetByTokenHash(ctx, presentedHash)
	if err != nil {
		if errors.Is(err, refreshtoken.ErrNotFound) {
			return RefreshResult{}, ErrRefreshRejected
		}
		return RefreshResult{}, fmt.Errorf("refresh: lookup family: %w", err)
	}

	// 3. Derive the successor. Deterministic (HMAC of family id and presented
	//    plaintext under a key derived from the JWT secret): only the hash is
	//    ever stored, and a replay inside the grace window yields the same one.
	mintPair, err := refreshmint.Successor(h.successorKey, family.ID(), cmd.RefreshTokenPlain)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: derive successor: %w", err)
	}

	// 4. Rotate inside UpdateByID closure.
	//
	//    Reuse-detection is the security-critical branch: when the
	//    presented hash matches a CONSUMED token, the aggregate revokes
	//    the entire family + emits RevokedEvent; the closure MUST
	//    return shouldPersist=true to commit that state, but the outer
	//    Handle returns ErrRefreshRejected.
	now := h.now()
	// The client is the request context's name for the caller (the same key the
	// rate limiters use); only an HMAC of it is stored.
	client := refreshmint.ClientBinding(h.bindingKey, family.ID(), argon2.ClientFrom(ctx))
	var rotateErr error
	var graceOutcome refreshtoken.RotationOutcome
	uerr := h.families.UpdateByID(ctx, family.ID(), func(f *refreshtoken.Family) (bool, error) {
		outcome, err := f.RotateWithGrace(refreshtoken.RotationParams{
			Presented:   presentedHash,
			Candidate:   mintPair.Hash,
			TTL:         h.refreshTTL,
			AbsoluteTTL: h.refreshTTL,
			GraceWindow: h.graceWindow,
			Client:      client,
			Now:         now,
		})
		graceOutcome = outcome
		if err == nil {
			// A grace replay moves one counter on the consumed token (persisted, under
			// the family row lock) and returns the same successor. Every later step
			// (person, membership, the family re-read) still runs, so a replay never
			// outlives a revocation.
			return true, nil
		}
		// Reuse-detection mutates the aggregate (revokes); persist that
		// mutation but propagate nothing — the outer Handle decides
		// whether to translate to ErrRefreshRejected.
		if errors.Is(err, refreshtoken.ErrReuseDetected) || errors.Is(err, refreshtoken.ErrSessionExpired) {
			rotateErr = err
			return true, nil
		}
		// Other rotate errors (revoked, expired, unknown-token): no
		// state change to persist; surface the error directly.
		rotateErr = err
		return false, nil
	})
	if uerr != nil {
		return RefreshResult{}, fmt.Errorf("refresh: persist rotation: %w", uerr)
	}
	h.recordGrace(ctx, family.ID(), graceOutcome)
	if rotateErr != nil {
		if errors.Is(rotateErr, refreshtoken.ErrSessionExpired) {
			return RefreshResult{}, fmt.Errorf("%w (%w)", ErrRefreshRejected, ErrSessionExpired)
		}
		return RefreshResult{}, ErrRefreshRejected
	}

	// 5. Resolve Person + Tenant + Membership for new JWT.
	p, err := h.persons.GetByID(ctx, family.PersonID())
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: resolve person: %w", err)
	}
	if !p.MaySignIn() {
		// Person was deactivated, anonymised or globally suspended AFTER family
		// creation (a suspension's cascade may lag the next refresh): kill the
		// family + reject, so the silent refresh never mints a token for them.
		_ = h.families.UpdateByID(ctx, family.ID(), func(f *refreshtoken.Family) (bool, error) {
			return true, f.Revoke("person-no-longer-active", now)
		})
		return RefreshResult{}, ErrRefreshRejected
	}

	tn, err := h.tenants.GetByID(ctx, family.TenantID())
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: resolve tenant: %w", err)
	}

	m, err := h.memberships.GetActiveForPerson(ctx, p.ID())
	if err != nil {
		// No Active Membership AT ALL → reject + revoke. Person changed
		// jobs and the new tenant's onboarding hasn't completed.
		_ = h.families.UpdateByID(ctx, family.ID(), func(f *refreshtoken.Family) (bool, error) {
			return true, f.Revoke("no-active-membership", now)
		})
		return RefreshResult{}, ErrRefreshRejected
	}
	if m.TenantID() != tn.ID() {
		// Active Membership moved to a DIFFERENT tenant — old family
		// must die; client must re-login under new tenant scope.
		_ = h.families.UpdateByID(ctx, family.ID(), func(f *refreshtoken.Family) (bool, error) {
			return true, f.Revoke("active-membership-changed-tenant", now)
		})
		return RefreshResult{}, ErrRefreshRejected
	}

	// 6. Resolve effective permissions + SuperUser flag — same shape
	//    as Login. Refresh re-emits the latest claims so a permission
	//    grant/revoke since the previous JWT propagates within
	//    AccessTokenTTL of the next refresh.
	//
	//    Tenant ctx binding is required: the resolver's downstream
	//    role-by-IDs query runs under TxScopeTenant, which expects
	//    `app.tenant_id` GUC set. Refresh is the entry point — no
	//    upstream middleware has bound it yet.
	ctx = tenancy.WithID(ctx, tenancy.ID(tn.ID().String()))
	authClaims, err := h.resolver.ResolveAuth(ctx, m)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: resolve permissions: %w", err)
	}

	// 7. Re-read the family AFTER the person was read. A password change revokes
	//    every family in the transaction that rotates the stamp; if the person
	//    read above already saw the new stamp, the revocation is visible here too,
	//    and a family that was rotated a moment before the change must not turn
	//    into an unflagged token for the new stamp. If the person read saw the old
	//    stamp, the token carries it and is stale on arrival.
	current, err := h.families.GetByID(ctx, family.ID())
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: re-read family: %w", err)
	}
	if current.IsRevoked() {
		return RefreshResult{}, ErrRefreshRejected
	}

	// 8. Issue new JWT.
	access, err := h.jwt.Issue(jwt.IssueArgs{
		PersonID:      p.ID().String(),
		TenantID:      tn.ID().String(),
		TenantSlug:    tn.Slug().String(),
		MembershipID:  m.ID().String(),
		SecurityStamp: p.SecurityStamp().String(),
		IsPlatform:    tn.Slug().String() == tenant.PlatformSlug, // match login.go — platform scope must survive refresh
		IsSuperUser:   authClaims.IsSuperUser,
		Permissions:   permissionNames(authClaims.Permissions),
		Rank:          rankClaim(authClaims.Rank),
		// Minted from the stored Person, like login: the flag survives refresh and
		// is gone with the first token after the password change.
		MustChangePassword: p.MustChangePassword(),
	})
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: issue jwt: %w", err)
	}

	return RefreshResult{
		AccessToken:          access,
		RefreshTokenPlain:    mintPair.Plaintext,
		AccessTokenExpiresAt: now.Add(jwt.AccessTokenTTL),
		PersonID:             p.ID(),
		TenantID:             tn.ID(),
		MembershipID:         m.ID(),
		MustChangePassword:   p.MustChangePassword(),
	}, nil
}

// recordGrace counts and logs a presentation of a just-rotated token (the three
// outcomes of RotationOutcome that are grace cases; a first rotation and every other
// kind of reuse are not). The refused kinds also left a family revocation with reason
// reuse_detected in the outbox (the SIEM subscriber's trigger): that is their audit
// event, so none is added here.
func (h RefreshHandler) recordGrace(ctx context.Context, id refreshtoken.FamilyID, outcome refreshtoken.RotationOutcome) {
	var label string
	switch outcome {
	case refreshtoken.RotationGraceReplay:
		label = graceOutcomeSameClient
	case refreshtoken.RotationRefusedOtherClient:
		label = graceOutcomeRefusedOtherClient
	case refreshtoken.RotationRefusedLimit:
		label = graceOutcomeRefusedLimit
	default:
		return
	}
	if h.graceReplays != nil {
		h.graceReplays.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", label)))
	}
	if h.log == nil {
		return
	}
	if outcome == refreshtoken.RotationGraceReplay {
		h.log.InfoContext(ctx, "refresh token replayed inside its grace window", "outcome", label, "family_id", id.String())
		return
	}
	h.log.WarnContext(ctx, "refresh token replay refused: the family is revoked", "outcome", label, "family_id", id.String())
}
