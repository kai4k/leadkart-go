package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
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
}

// ----- Handler errors --------------------------------------------------------

// ErrRefreshRejected is the SINGLE error for every refresh-failure
// path: unknown hash, revoked family, expired token, reuse detected.
// Identical shape defeats family-id enumeration. Reuse-detected
// causes the family to be revoked as a side effect (RFC 9700 §4.13).
var ErrRefreshRejected = errors.New("refresh: rejected")

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
	refreshTTL  time.Duration
}

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
) RefreshHandler {
	if now == nil {
		now = time.Now
	}
	return RefreshHandler{
		families:    families,
		persons:     persons,
		memberships: memberships,
		tenants:     tenants,
		resolver:    resolver,
		jwt:         jwtIssuer,
		now:         now,
		refreshTTL:  refreshTTL,
	}
}

// Handle executes the rotation flow:
//
//  1. Hash the presented plaintext.
//  2. Look up family by hash → ErrRefreshRejected if not found.
//  3. Mint new ⟨plaintext, hash⟩ pair.
//  4. UpdateByID closure runs Family.Rotate(presentedHash, newHash).
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

	// 3. Mint replacement.
	mintPair, err := refreshmint.Mint()
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: mint: %w", err)
	}

	// 4. Rotate inside UpdateByID closure.
	//
	//    Reuse-detection is the security-critical branch: when the
	//    presented hash matches a CONSUMED token, the aggregate revokes
	//    the entire family + emits RevokedEvent; the closure MUST
	//    return shouldPersist=true to commit that state, but the outer
	//    Handle returns ErrRefreshRejected.
	now := h.now()
	var rotateErr error
	uerr := h.families.UpdateByID(ctx, family.ID(), func(f *refreshtoken.Family) (bool, error) {
		err := f.Rotate(presentedHash, mintPair.Hash, h.refreshTTL, now)
		if err == nil {
			return true, nil
		}
		// Reuse-detection mutates the aggregate (revokes); persist that
		// mutation but propagate nothing — the outer Handle decides
		// whether to translate to ErrRefreshRejected.
		if errors.Is(err, refreshtoken.ErrReuseDetected) {
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
	if rotateErr != nil {
		return RefreshResult{}, ErrRefreshRejected
	}

	// 5. Resolve Person + Tenant + Membership for new JWT.
	p, err := h.persons.GetByID(ctx, family.PersonID())
	if err != nil {
		return RefreshResult{}, fmt.Errorf("refresh: resolve person: %w", err)
	}
	if !p.IsActive() || p.IsAnonymised() {
		// Person was deactivated/anonymised AFTER family creation —
		// kill the family + reject.
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

	// 7. Issue new JWT.
	access, err := h.jwt.Issue(jwt.IssueArgs{
		PersonID:      p.ID().String(),
		TenantID:      tn.ID().String(),
		TenantSlug:    tn.Slug().String(),
		MembershipID:  m.ID().String(),
		SecurityStamp: p.SecurityStamp().String(),
		IsPlatform:    false, // Platform tenant lands in v0.3
		IsSuperUser:   authClaims.IsSuperUser,
		Permissions:   permissionNames(authClaims.Permissions),
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
	}, nil
}
