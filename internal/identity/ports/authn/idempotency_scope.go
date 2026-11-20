package authn

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
)

// authRoutePrefix is the credential surface: login, refresh, password reset,
// change-password and the rest of /api/v1/auth/. Idempotency never applies there
// (see [IdempotencyCallerKeyer]).
const authRoutePrefix = "/api/v1/auth/"

// Markers the scope hash records for an X-Tenant-Id the scope middleware will
// refuse. They are constants on purpose: every malformed value shares ONE scope,
// which always ends in a 4xx that is never stored, so a caller cannot mint
// distinct scopes by varying garbage.
const (
	scopeHeaderInvalid  = "invalid"
	scopeHeaderMultiple = "multiple"
)

// IdempotencyCallerKeyer scopes X-Command-Id records (ADR 0068 D11).
//
// The idempotency middleware runs in the host chain, before any route's own
// authentication, so the request context carries no tenant yet. Scoping by that
// context (the old default) put every caller behind the BFF into one
// "anon:<bff address>" namespace: tenant A and tenant B choosing the same key on
// the same body were served each other's stored response.
//
// This keyer verifies the bearer token itself (signature, expiry and algorithm
// only; stamp freshness, session liveness and permissions remain the route's job,
// and a refused request is never stored because only 2xx answers are) and returns
//
//	tenant:<tenant_id>:<hash(person, membership, impersonation session, tenant scope, METHOD path)>
//
// so one tenant, one person (membership), one session, one acted-on tenant and one
// route can never replay or collide with another's. The tenant scope is the
// X-Tenant-Id an OPERATOR sends (canonicalised with the scope middleware's own
// [CanonicalID]; equal to the operator's home tenant counts as absent): the same
// operator acting in tenant A and then tenant B with the same key, body and path
// must run twice, not be handed A's stored response. The header is read only for
// a platform operator, the only caller the scope middleware honours it for; for
// everyone else it is ignored there, so it must not be able to change the scope
// here (varying it would defeat replay protection). A value the scope middleware
// will refuse (not a UUID, several values) hashes as a constant marker, so all
// such requests share one scope that ends in a 4xx, which is never stored.
//
// The keyer runs before the scope middleware (it sits in the host chain), so it
// validates the header itself and decides nothing about authorisation.
//
// Not eligible (the keyer returns [idempotency.Ineligible]; the middleware ignores
// X-Command-Id and stores nothing):
//
//   - everything under /api/v1/auth/: login and refresh answers carry plaintext
//     refresh tokens, and the stored hash of a body containing a password has no
//     business in a table; these are credential exchanges, not commands to replay;
//   - every anonymous request (no verifiable bearer token): there is no caller to
//     scope to, and replaying one client's answer to another that sent the same
//     key is exactly the cross-caller leak the scope exists to prevent.
//
// The hash keeps the scope at a fixed width under the store's 200-byte limit.
func IdempotencyCallerKeyer(verifier Verifier) idempotency.CallerKeyer {
	if verifier == nil {
		panic("authn: IdempotencyCallerKeyer verifier required")
	}
	return func(r *http.Request) string {
		if strings.HasPrefix(r.URL.Path, authRoutePrefix) {
			return idempotency.Ineligible
		}
		token := extractBearer(r.Header)
		if token == "" {
			return idempotency.Ineligible
		}
		c, err := verifier.Verify(token)
		if err != nil || c == nil || strings.TrimSpace(c.TenantID) == "" {
			return idempotency.Ineligible
		}
		session := ""
		if c.Act != nil {
			session = c.Act.SessionID
		}
		return "tenant:" + strings.TrimSpace(c.TenantID) + ":" +
			scopeHash(c.Subject, c.MembershipID, session, tenantScopeOf(r, c), r.Method+" "+r.URL.Path)
	}
}

// tenantScopeOf is the acted-on tenant this request names, "" when it names none
// (or only the caller's own). Blank values count as absent, exactly as the scope
// middleware reads them.
func tenantScopeOf(r *http.Request, c *jwt.Claims) string {
	if !IsPlatformOperator(c) {
		return ""
	}
	values := r.Header.Values(TenantScopeHeader)
	if !anyNonBlank(values) {
		return ""
	}
	if len(values) > 1 {
		return scopeHeaderMultiple
	}
	id, ok := CanonicalID(values[0])
	if !ok {
		return scopeHeaderInvalid
	}
	if sameID(id, c.TenantID) {
		return ""
	}
	return id
}

// scopeHash is a collision-safe 128-bit digest of the parts (NUL-separated, so
// ("ab","c") and ("a","bc") differ), hex-encoded.
func scopeHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
