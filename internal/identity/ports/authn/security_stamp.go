package authn

import (
	"context"
	"net/http"
	"strings"
)

// StampValidator is the consumer-side interface RequireFreshStamp needs
// from a [adapters.SecurityStampValidator]. Declared here per Cheney
// "accept interfaces, return structs" so middleware tests can substitute
// in-memory fakes without standing up Redis + miniredis just to assert
// the composition shape.
//
// IsFresh contract:
//   - (true, nil)   — stored stamp matches claim → continue.
//   - (false, nil)  — stored stamp differs OR personID is empty → 401 stale.
//   - (_, err)      — repository / cache transport failure → 401 unauthenticated.
type StampValidator interface {
	IsFresh(ctx context.Context, personID string, claimStamp string) (bool, error)
}

// errCodeStaleToken signals that the bearer's signature/expiry checks
// passed, but the carried `security_stamp` claim no longer matches the
// stored stamp. Distinct from `unauthenticated` so the SPA can
// differentiate "your token is bad" vs. "your session was revoked
// out from under you" (e.g. password rotation on another device,
// admin global-suspend) and prompt re-login with the right copy.
//
// Auth0 / Okta canon: rotating a session-validation key + refresh
// window of ~30s. Stripe API "session expired" precedent — distinct
// status from raw 401-invalid-token.
const errCodeStaleToken = "stale_token"

// RequireFreshStamp gates a handler on a verified JWT AND a fresh
// security stamp. Composes [RequireAuth] (signature + expiry + tenant-
// claim binding) with a per-request stamp-vs-stored comparison.
//
// Reaction times:
//   - Fast path (cache hit on the validator's HybridCache facade):
//     ≈microseconds — L1 ristretto + atomic claim compare.
//   - Slow path (cache miss): one Postgres roundtrip via the facade's
//     read-through factory, singleflight-coalesced across concurrent
//     misses for the same Person.
//
// Why this exists:
//
// JWT signature alone cannot revoke a session in-flight — the signing
// key is valid until rotation, and rotation is rare (security.md key-
// history grace window). The security-stamp claim is the per-Person
// nonce minted at every credential-rotation event (password/email
// change, anonymisation, global suspend); rotating it on the stored
// Person while leaving the in-flight JWT unrotated is what makes the
// session "revoked" in <=30s without a stateful blacklist on every
// request.
//
// Subscribers downstream of those rotation events ALSO call
// SecurityStampCache.Invalidate to close the eventual-consistency
// window faster than the 30s TTL would (defense-in-depth — a
// forwarder-lag spike doesn't extend the staleness window).
//
// Doctrine sources:
//   - `audit-checklist.md §12b` "Cache facade per concern" — the
//     validator wraps a typed facade, never raw HybridCache.
//   - `security.md` "SecurityStamp rotation triggers" — what mutates
//     the stamp + therefore which subscribers must Invalidate the cache.
//   - Auth0 / Okta session-validation refresh window (~30s) — the TTL
//     fallback when subscriber invalidation hasn't propagated yet.
//
// Use this middleware on every authenticated route that mutates
// security-relevant state OR returns the caller's identity/profile.
// Routes that ARE the rotation event itself (e.g. POST /auth/refresh,
// which mints a new family + rotates the stamp on the Person) MUST
// keep RequireAuth — gating refresh on a freshness check it itself
// invalidates is an obvious re-entry trap.
func RequireFreshStamp(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler {
	if verifier == nil {
		panic("authn: RequireFreshStamp verifier required")
	}
	if validator == nil {
		panic("authn: RequireFreshStamp validator required")
	}
	auth := RequireAuth(verifier)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				// Defensive: RequireAuth should have either populated or
				// short-circuited; this branch is a wiring bug. Treat as
				// unauthenticated rather than panic — failing closed beats
				// crashing the server.
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"missing claims")
				return
			}
			personID := strings.TrimSpace(c.Subject)
			claimStamp := strings.TrimSpace(c.SecurityStamp)
			if personID == "" || claimStamp == "" {
				// A token with empty sub or security_stamp is a JWT-issuance
				// bug: every production-issued token populates both. Treat
				// as unauthenticated — defense-in-depth against an attacker
				// who crafts/strips the stamp claim after compromising the
				// signing key (RFC 8725 §3.1 alg-confusion mitigation lives
				// in jwt.Verify; this is the second layer).
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"token missing sub or security_stamp claim")
				return
			}
			fresh, err := validator.IsFresh(r.Context(), personID, claimStamp)
			if err != nil {
				// Repository / cache-transport failure. Fail closed per
				// `security.md` "fail closed on auth" — operators read the
				// upstream platform-middleware log row to triage. The
				// generic 401 shape matches RequireAuth's verify-failure
				// path so an attacker can't probe "did the validator hit
				// an internal error vs. a real stale token?" via response
				// differentiation.
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"stamp validation failed")
				return
			}
			if !fresh {
				writeError(w, http.StatusUnauthorized, errCodeStaleToken,
					"session has been revoked")
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}
