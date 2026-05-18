package ports

import (
	"log/slog"
	"net/http"

	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// handleGetCapabilities serves GET /v1/auth/me/capabilities.
//
// Returns the resolved permission/role bundle for the calling
// membership so the frontend never has to decode the JWT to drive nav
// / tier / button-visibility. Auth0 /userinfo + Microsoft Graph /me
// canonical shape.
//
// Implementation note (ADR 0036 + Phase 1.5 hardening):
//
// Every field returned here comes from the verified JWT claims —
// zero DB hit on the read path. The JWT carries:
//   - sub                  → person_id
//   - membership_id        → membership_id
//   - tenant_id            → tenant_id
//   - tenant_slug          → tenant_slug
//   - is_platform          → is_platform (already slug-anchored at issuance per
//                            Phase 1.5 — is_platform=true only if
//                            tenant_slug == "platform" at login mint)
//   - is_super_user        → is_super_user
//   - permission           → permissions[] (closed-set catalogue strings)
//
// Why this is correct without re-resolving from DB:
//
//   - Permissions on the JWT are PermissionResolver.ResolveAuth output
//     at login + every refresh. Rotation triggers (role grant/revoke,
//     suspend, anonymise) rotate security_stamp, which forces a refresh
//     ≤ 30s later via the existing RequireFreshStamp middleware. So
//     stale claims here are bounded by the same SLA the rest of the
//     authorization surface uses.
//   - Profile enrichment (email, first_name, last_name, role NAMES
//     instead of just permission strings) lands in v0.3 — gated on a
//     HybridCache keyed by (membership_id, security_stamp) so the
//     enrichment is sub-1ms cached + auto-invalidates on stamp
//     rotation.
//
// Auth: REQUIRES freshness-checked JWT (RequireFreshStamp). Anonymous
// callers don't have a membership.
func handleGetCapabilities(log *slog.Logger) http.Handler {
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

		writeJSON(w, http.StatusOK, CapabilitiesDto{
			PersonID:     c.Subject,
			MembershipID: c.MembershipID,
			TenantID:     c.TenantID,
			TenantSlug:   c.TenantSlug,
			IsPlatform:   c.IsPlatform,
			IsSuperUser:  c.IsSuperUser,
			Permissions:  perms,
		})
	})
}
