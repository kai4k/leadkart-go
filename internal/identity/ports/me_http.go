package ports

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// handleGetCapabilities serves GET /v1/auth/me/capabilities.
//
// Returns the resolved permission/role/profile bundle for the calling
// membership so the frontend never has to decode the JWT to drive
// nav / tier / button-visibility. Auth0 /userinfo + Microsoft Graph
// /me canonical shape.
//
// JWT-resident fields (no DB hit):
//   - sub                  → person_id
//   - membership_id        → membership_id
//   - tenant_id            → tenant_id
//   - tenant_slug          → tenant_slug
//   - is_platform          → is_platform (slug-anchored at issuance
//     per Phase 1.5 — true only if
//     tenant_slug == "platform")
//   - is_super_user        → is_super_user
//   - permission           → permissions[] (closed-set catalogue)
//
// Enriched fields (cached via CapabilitiesTTL — per ADR 0042):
//   - email, first_name, last_name (from Person)
//   - roles[] {id, name, is_super_admin} (from Membership +
//     RoleRepo.GetByIDs)
//
// Cache key includes the security_stamp so any rotation trigger
// (role grant/revoke, password change, anonymise, global suspend)
// invalidates the entry by changing the key — TTL is just a memory
// bound. Stale enrichment is bounded by the same security_stamp
// freshness contract the rest of the authorization surface uses.
//
// On enrichment-resolver failure the handler degrades to the
// JWT-only projection (no email/name/roles) rather than 500ing the
// request — capabilities is a UX driver, not load-bearing for
// authorization (which is permissions[], straight from the JWT).
//
// Auth: REQUIRES freshness-checked JWT (RequireFreshStamp).
func handleGetCapabilities(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			// Defensive — RequireFreshStamp should have short-circuited.
			// Reaching here means a wiring bug; log + 500 to surface it.
			log.ErrorContext(r.Context(), "capabilities: missing claims in authenticated handler")
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		// Defensive copy of the permission slice — the JWT claims live
		// in the request context and may be referenced after this
		// handler returns. Returning the underlying slice directly
		// would let a downstream mutation corrupt cached claims.
		//
		// Always non-nil ([]string{} vs nil) so JSON serializes as
		// `"permissions": []` not `"permissions": null` — Stripe /
		// Auth0 convention; frontends can `.length` / `.includes()`
		// without nil checks. make([]string, n) where n==0 yields a
		// non-nil zero-length slice, which is exactly what we want.
		perms := make([]string, len(c.Permissions))
		copy(perms, c.Permissions)

		actor := authn.AuthorityFrom(r.Context())
		dto := CapabilitiesDto{
			PersonID:             c.Subject,
			MembershipID:         c.MembershipID,
			TenantID:             c.TenantID,
			TenantSlug:           c.TenantSlug,
			IsPlatform:           c.IsPlatform,
			IsSuperUser:          c.IsSuperUser,
			MustChangePassword:   c.MustChangePassword,
			Permissions:          perms,
			EffectivePermissions: authn.EffectivePermissions(c),
			Roles:                []CapabilityRoleDto{}, // non-nil; populated below
			Rank:                 actor.Rank().Int(),
			Tier:                 string(actor.Rank().Tier()),
			PasswordPolicy:       passwordRules(r.Context(), log, a, tenant.ID(c.TenantID)),
		}

		if c.Act != nil {
			dto.Impersonation = impersonationContext(r.Context(), log, a, c.Act.SessionID, tenant.ID(c.TenantID))
		}

		// Enrichment via the cached query handler — populates email +
		// first_name + last_name + role names. Cache hit returns
		// sub-millisecond; miss runs the Person + Membership +
		// Roles.GetByIDs hydration.
		view, err := a.Queries.GetCapabilities.Handle(r.Context(), query.GetCapabilitiesQuery{
			PersonID:      person.ID(c.Subject),
			MembershipID:  membership.ID(c.MembershipID),
			TenantID:      tenant.ID(c.TenantID),
			SecurityStamp: c.SecurityStamp,
		})
		if err != nil {
			// Degraded path — log + return the JWT-only projection.
			// The frontend still gets the load-bearing authorization
			// surface (permissions[]); only the cosmetic enrichment
			// is missing. Operators see the failure in logs.
			log.WarnContext(r.Context(), "capabilities: enrichment failed, returning JWT-only projection", "err", err)
		} else {
			dto.Email = view.Email
			dto.FirstName = view.FirstName
			dto.LastName = view.LastName
			roles := make([]CapabilityRoleDto, 0, len(view.Roles))
			for _, role := range view.Roles {
				roles = append(roles, CapabilityRoleDto{
					ID:           role.ID,
					Name:         role.Name,
					IsSuperAdmin: role.IsSuperAdmin,
				})
			}
			dto.Roles = roles
		}

		writeJSON(w, http.StatusOK, dto)
	})
}

// impersonationContext names the session the token belongs to and the company it
// acts in (the token's own tenant): the one session this token can ever see. The
// name is read fresh; when the tenant cannot be read the session id still answers
// and the name is empty (the banner then falls back to its generic wording).
func impersonationContext(ctx context.Context, log *slog.Logger, a app.Application, sessionID string, tenantID tenant.ID) *ImpersonationContextDto {
	dto := &ImpersonationContextDto{SessionID: sessionID}
	view, err := a.Queries.GetTenant.Handle(ctx, query.GetTenantQuery{TenantID: tenantID})
	if err != nil {
		log.WarnContext(ctx, "capabilities: impersonated tenant name unavailable", "err", err)
		return dto
	}
	dto.TenantDisplayName = view.DisplayName
	return dto
}

// passwordRules is the caller's own tenant's password policy as the wire shape: the
// tenant of the TOKEN (never an operator's acted-on scope, which this route ignores),
// the default policy when the tenant row is gone, the same rule change-password
// applies. An impersonation session carries the impersonated tenant as its own, so it
// sees that tenant's rules (change-password itself is refused to it). On a read
// failure the default rules are returned and the failure logged: the form is a
// courtesy, the server enforces whatever the policy really is and answers 422 with
// the failed rule codes.
func passwordRules(ctx context.Context, log *slog.Logger, a app.Application, tenantID tenant.ID) PasswordRulesDto {
	view, err := a.Queries.GetOwnPasswordPolicy.Handle(ctx, tenantID)
	if err != nil {
		log.WarnContext(ctx, "capabilities: password policy unavailable, returning the default rules", "err", err)
		view, _ = query.GetOwnPasswordPolicyHandler{}.Handle(ctx, "") // the zero handler answers the default
	}
	return PasswordRulesDto{
		MinLength:         view.MinLength,
		MaxLength:         view.MaxLength,
		RequiresUppercase: view.RequiresUppercase,
		RequiresLowercase: view.RequiresLowercase,
		RequiresDigit:     view.RequiresDigit,
		RequiresSymbol:    view.RequiresSymbol,
	}
}
